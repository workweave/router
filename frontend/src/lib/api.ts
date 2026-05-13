const BASE = "/admin/v1";

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    ...((init?.headers as Record<string, string>) ?? {}),
  };
  const res = await fetch(`${BASE}${path}`, {
    ...init,
    credentials: "include",
    headers,
  });
  if (res.status === 401 && typeof window !== "undefined") {
    // Session expired or never authenticated — bounce to login. The
    // (app) layout's auth gate handles the same case on initial load,
    // but in-flight calls (chart refreshes, mutations) need their own
    // bounce so users don't sit on a broken page.
    if (!window.location.pathname.startsWith("/ui/login")) {
      // Strip the /ui basePath so Next's router.replace() doesn't re-prepend
      // it after login (which would yield /ui/ui/...). Anything outside the
      // basePath isn't an app path; default to /dashboard.
      const path = window.location.pathname;
      const internal = path.startsWith("/ui/") ? path.slice(3) : "/dashboard";
      const next = encodeURIComponent(internal);
      window.location.href = `/ui/login?next=${next}`;
      throw new Error("401: redirecting to login");
    }
  }
  if (!res.ok) {
    const body = await res.text();
    throw new Error(`${res.status}: ${body}`);
  }
  if (res.status === 204) return undefined as T;
  return res.json() as Promise<T>;
}

// --- types ---

export interface MetricsSummary {
  request_count: number;
  total_tokens: number;
  total_requested_cost_usd: number;
  total_actual_cost_usd: number;
  total_savings_usd: number;
}

export interface TimeseriesBucket {
  bucket: string;
  requested_cost_usd: number;
  actual_cost_usd: number;
}

export interface MetricsTimeseries {
  buckets: TimeseriesBucket[];
}

export interface MetricsDetailRow {
  timestamp: string;
  request_id: string;
  requested_model: string;
  decision_model: string;
  decision_provider: string;
  decision_reason: string;
  sticky_hit: boolean;
  input_tokens: number;
  output_tokens: number;
  requested_cost_usd: number;
  actual_cost_usd: number;
  total_latency_ms: number;
  upstream_status_code: number;
}

export interface MetricsDetails {
  rows: MetricsDetailRow[];
}

export interface APIKey {
  id: string;
  name: string | null;
  key_prefix: string;
  key_suffix: string;
  last_used_at: string | null;
  created_at: string;
}

export interface IssueAPIKeyResponse {
  key: APIKey;
  token: string;
}

export interface ExternalKey {
  id: string;
  provider: string;
  name: string | null;
  key_prefix: string;
  key_suffix: string;
  last_used_at: string | null;
  created_at: string;
}

export interface RouterConfig {
  cluster_version: string;
  embed_last_user_message: boolean;
  sticky_decision_ttl_ms: string;
  otel_enabled: boolean;
  dev_mode: boolean;
  semantic_cache_enabled: boolean;
  env_provider_keys: string[];
}

export interface MeResponse {
  authenticated: boolean;
  subject?: string;
}

// --- API calls ---

export const api = {
  auth: {
    me: () => request<MeResponse>("/auth/me"),
    login: (password: string) =>
      request<{ ok: boolean; expires_at: string }>("/auth/login", {
        method: "POST",
        body: JSON.stringify({ password }),
      }),
    logout: () => request<{ ok: boolean }>("/auth/logout", { method: "POST" }),
  },
  metrics: {
    summary: (from?: string, to?: string) => {
      const params = new URLSearchParams();
      if (from) params.set("from", from);
      if (to) params.set("to", to);
      const qs = params.toString();
      return request<MetricsSummary>(`/metrics/summary${qs ? `?${qs}` : ""}`);
    },
    timeseries: (granularity: "hour" | "day" | "week", from?: string, to?: string) => {
      const params = new URLSearchParams({ granularity });
      if (from) params.set("from", from);
      if (to) params.set("to", to);
      return request<MetricsTimeseries>(`/metrics/timeseries?${params.toString()}`);
    },
    details: (from: string, to: string, limit: number = 100) => {
      const params = new URLSearchParams({ from, to, limit: String(limit) });
      return request<MetricsDetails>(`/metrics/details?${params.toString()}`);
    },
  },
  keys: {
    list: () => request<{ keys: APIKey[] }>("/keys"),
    issue: (name?: string) =>
      request<IssueAPIKeyResponse>("/keys", {
        method: "POST",
        body: JSON.stringify({ name: name ?? "" }),
      }),
    // Soft-deletes the current active key and issues a replacement in one
    // round-trip. The previous token stops working immediately. Carries
    // forward the previous key's name server-side.
    rotate: () =>
      request<IssueAPIKeyResponse>("/keys/rotate", { method: "POST" }),
    delete: (id: string) => request<void>(`/keys/${id}`, { method: "DELETE" }),
  },
  providerKeys: {
    list: () => request<{ keys: ExternalKey[] }>("/provider-keys"),
    upsert: (provider: string, key: string, name?: string) =>
      request<ExternalKey>("/provider-keys", {
        method: "POST",
        body: JSON.stringify({ provider, key, name }),
      }),
    delete: (id: string) => request<void>(`/provider-keys/${id}`, { method: "DELETE" }),
  },
  config: {
    get: () => request<RouterConfig>("/config"),
  },
};
