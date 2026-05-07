"use client";

import { Suspense, useEffect, useState } from "react";
import { Copy, Trash2 } from "lucide-react";
import { useRouter, useSearchParams } from "next/navigation";

import { Button } from "@/components/Button";
import { Card } from "@/components/Card";
import { Input, Select } from "@/components/Input";
import { PageBody, PageHeader } from "@/components/PageHeader";
import { cn } from "@/lib/cn";
import {
  api,
  type APIKey,
  type ExternalKey,
  type RouterConfig,
} from "@/lib/api";

type TabId = "router-keys" | "provider-keys" | "config";

const TABS: Array<{ id: TabId; label: string; description: string }> = [
  {
    id: "router-keys",
    label: "Router API keys",
    description: "Keys used to authenticate requests to this router.",
  },
  {
    id: "provider-keys",
    label: "Provider API keys",
    description: "Bring your own keys for Anthropic, OpenAI, Google, OpenRouter.",
  },
  {
    id: "config",
    label: "Configuration",
    description: "Runtime values set via environment variables.",
  },
];

function isTabId(v: string | null): v is TabId {
  return v === "router-keys" || v === "provider-keys" || v === "config";
}

export default function SettingsPage() {
  return (
    <Suspense fallback={null}>
      <SettingsInner />
    </Suspense>
  );
}

function SettingsInner() {
  const params = useSearchParams();
  const router = useRouter();
  const tabParam = params.get("tab");
  const tab: TabId = isTabId(tabParam) ? tabParam : "router-keys";
  const active = TABS.find((t) => t.id === tab) ?? TABS[0];

  function setTab(next: TabId) {
    const sp = new URLSearchParams(params.toString());
    sp.set("tab", next);
    router.replace(`/settings?${sp.toString()}`, { scroll: false });
  }

  return (
    <>
      <PageHeader title="Settings" description={active.description} />
      <div className="flex items-center gap-1 border-b border-border px-8">
        {TABS.map((t) => (
          <button
            key={t.id}
            type="button"
            onClick={() => setTab(t.id)}
            aria-selected={tab === t.id}
            className={cn(
              "relative -mb-px border-b-2 border-transparent px-3 py-2.5 text-xs font-medium text-muted-foreground transition-colors hover:text-foreground",
              "aria-selected:border-foreground aria-selected:text-foreground",
            )}
          >
            {t.label}
          </button>
        ))}
      </div>
      <PageBody>
        {tab === "router-keys" && <RouterKeysPanel />}
        {tab === "provider-keys" && <ProviderKeysPanel />}
        {tab === "config" && <ConfigPanel />}
      </PageBody>
    </>
  );
}

// ──────────────────────────────────────────────────────────────────────────
// Router keys panel
// ──────────────────────────────────────────────────────────────────────────

function RouterKeysPanel() {
  const [keys, setKeys] = useState<APIKey[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [name, setName] = useState("");
  const [creating, setCreating] = useState(false);
  const [deleting, setDeleting] = useState<string | null>(null);
  const [newToken, setNewToken] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);

  function load() {
    api.keys
      .list()
      .then((r) => setKeys(r.keys ?? []))
      .catch((err: unknown) =>
        setError(err instanceof Error ? err.message : "Failed to load keys"),
      );
  }

  useEffect(load, []);

  async function handleCreate(e: React.FormEvent) {
    e.preventDefault();
    setCreating(true);
    try {
      const res = await api.keys.issue(name.trim() || undefined);
      setNewToken(res.token);
      setName("");
      load();
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : "Failed to create key");
    } finally {
      setCreating(false);
    }
  }

  async function handleDelete(id: string) {
    setDeleting(id);
    try {
      await api.keys.delete(id);
      load();
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : "Failed to delete key");
    } finally {
      setDeleting(null);
    }
  }

  function handleCopy() {
    if (!newToken) return;
    navigator.clipboard.writeText(newToken).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    });
  }

  return (
    <>
      {error && <ErrorBanner>{error}</ErrorBanner>}

      {newToken && (
        <div className="rounded-lg border border-success/30 bg-success/5 p-4">
          <p className="mb-2 text-xs font-medium text-success">
            Key created — copy it now, it won&apos;t be shown again.
          </p>
          <div className="flex items-center gap-2">
            <code className="flex-1 rounded bg-muted px-3 py-1.5 font-mono text-2xs text-foreground">
              {newToken}
            </code>
            <Button variant="outline" size="sm" onClick={handleCopy}>
              <Copy size={12} />
              {copied ? "Copied" : "Copy"}
            </Button>
          </div>
          <button
            className="mt-2 text-2xs text-muted-foreground underline"
            onClick={() => setNewToken(null)}
          >
            Dismiss
          </button>
        </div>
      )}

      <Card title="Issue a new key">
        <form onSubmit={handleCreate} className="flex items-end gap-3">
          <div className="flex-1">
            <Input
              label="Name (optional)"
              placeholder="My API key"
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </div>
          <Button type="submit" variant="filled" disabled={creating}>
            {creating ? "Creating…" : "Create key"}
          </Button>
        </form>
      </Card>

      {keys.length > 0 ? (
        <Card title="Active router keys" contentClassName="p-0">
          <ul className="divide-y divide-border">
            {keys.map((k) => (
              <li key={k.id} className="flex items-center justify-between px-5 py-3">
                <div>
                  <div className="text-xs font-medium text-foreground">
                    {k.name ?? "Unnamed key"}
                  </div>
                  <p className="mt-0.5 font-mono text-2xs text-muted-foreground">
                    {k.key_prefix}…{k.key_suffix}
                    <span className="ml-2 font-sans">
                      · created {formatDate(k.created_at)}
                      {k.last_used_at && ` · last used ${formatDate(k.last_used_at)}`}
                    </span>
                  </p>
                </div>
                <Button
                  variant="ghost"
                  size="icon"
                  onClick={() => handleDelete(k.id)}
                  disabled={deleting === k.id}
                  className="text-muted-foreground hover:text-danger"
                >
                  <Trash2 size={14} />
                </Button>
              </li>
            ))}
          </ul>
        </Card>
      ) : (
        <EmptyHint>No router keys yet.</EmptyHint>
      )}
    </>
  );
}

// ──────────────────────────────────────────────────────────────────────────
// Provider keys panel
// ──────────────────────────────────────────────────────────────────────────

const PROVIDERS = ["anthropic", "openai", "google", "openrouter"] as const;
type Provider = (typeof PROVIDERS)[number];

function ProviderKeysPanel() {
  const [keys, setKeys] = useState<ExternalKey[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [provider, setProvider] = useState<Provider>("anthropic");
  const [keyValue, setKeyValue] = useState("");
  const [name, setName] = useState("");
  const [saving, setSaving] = useState(false);
  const [deleting, setDeleting] = useState<string | null>(null);

  function load() {
    api.providerKeys
      .list()
      .then((r) => setKeys(r.keys ?? []))
      .catch((err: unknown) =>
        setError(err instanceof Error ? err.message : "Failed to load keys"),
      );
  }

  useEffect(load, []);

  async function handleSave(e: React.FormEvent) {
    e.preventDefault();
    if (!keyValue.trim()) return;
    setSaving(true);
    try {
      await api.providerKeys.upsert(provider, keyValue.trim(), name.trim() || undefined);
      setKeyValue("");
      setName("");
      load();
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : "Failed to save key");
    } finally {
      setSaving(false);
    }
  }

  async function handleDelete(id: string) {
    setDeleting(id);
    try {
      await api.providerKeys.delete(id);
      load();
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : "Failed to delete key");
    } finally {
      setDeleting(null);
    }
  }

  return (
    <>
      {error && <ErrorBanner>{error}</ErrorBanner>}

      <Card title="Add or replace a key">
        <form onSubmit={handleSave} className="space-y-3">
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-[160px_1fr]">
            <Select
              label="Provider"
              value={provider}
              onChange={(e) => setProvider(e.target.value as Provider)}
            >
              {PROVIDERS.map((p) => (
                <option key={p} value={p}>
                  {p.charAt(0).toUpperCase() + p.slice(1)}
                </option>
              ))}
            </Select>
            <Input
              label="API key"
              type="password"
              placeholder="sk-..."
              value={keyValue}
              onChange={(e) => setKeyValue(e.target.value)}
              required
            />
          </div>
          <Input
            label="Name (optional)"
            placeholder="My Anthropic key"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
          <div>
            <Button type="submit" variant="filled" disabled={saving || !keyValue.trim()}>
              {saving ? "Saving…" : "Save key"}
            </Button>
          </div>
        </form>
      </Card>

      {keys.length > 0 ? (
        <Card title="Active provider keys" contentClassName="p-0">
          <ul className="divide-y divide-border">
            {keys.map((k) => (
              <li key={k.id} className="flex items-center justify-between px-5 py-3">
                <div>
                  <div className="flex items-center gap-2">
                    <span className="text-xs font-medium capitalize text-foreground">
                      {k.provider}
                    </span>
                    {k.name && (
                      <span className="text-2xs text-muted-foreground">· {k.name}</span>
                    )}
                  </div>
                  <p className="mt-0.5 font-mono text-2xs text-muted-foreground">
                    {k.key_prefix}…{k.key_suffix}
                  </p>
                </div>
                <Button
                  variant="ghost"
                  size="icon"
                  onClick={() => handleDelete(k.id)}
                  disabled={deleting === k.id}
                  className="text-muted-foreground hover:text-danger"
                >
                  <Trash2 size={14} />
                </Button>
              </li>
            ))}
          </ul>
        </Card>
      ) : (
        <EmptyHint>No provider keys configured.</EmptyHint>
      )}
    </>
  );
}

// ──────────────────────────────────────────────────────────────────────────
// Config panel
// ──────────────────────────────────────────────────────────────────────────

interface ConfigRow {
  label: string;
  value: string;
  description: string;
}

function buildRows(cfg: RouterConfig): ConfigRow[] {
  return [
    {
      label: "Cluster version",
      value: cfg.cluster_version || "—",
      description: "Active routing artifact bundle served by default",
    },
    {
      label: "Embed last user message",
      value: cfg.embed_last_user_message ? "Enabled" : "Disabled",
      description: "Whether the router embeds only the last user turn for cluster routing",
    },
    {
      label: "Sticky decision TTL",
      value: cfg.sticky_decision_ttl_ms || "—",
      description: "How long a sticky routing decision is cached per conversation",
    },
    {
      label: "Semantic cache",
      value: cfg.semantic_cache_enabled ? "Enabled" : "Disabled",
      description: "Whether semantic response caching is active",
    },
    {
      label: "OpenTelemetry",
      value: cfg.otel_enabled ? "Enabled" : "Disabled",
      description: "Whether OTEL tracing and metrics are exported",
    },
    {
      label: "Dev mode",
      value: cfg.dev_mode ? "On" : "Off",
      description: "Relaxed auth and verbose logging — never enable in production",
    },
  ];
}

function ConfigPanel() {
  const [config, setConfig] = useState<RouterConfig | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    api.config
      .get()
      .then(setConfig)
      .catch((err: unknown) =>
        setError(err instanceof Error ? err.message : "Failed to load config"),
      );
  }, []);

  return (
    <>
      {error && <ErrorBanner>{error}</ErrorBanner>}

      <Card title="Runtime values" contentClassName="p-0">
        {config == null ? (
          <div className="px-5 py-8 text-center text-2xs text-muted-foreground">
            {error ? "Failed to load" : "Loading…"}
          </div>
        ) : (
          <ul className="divide-y divide-border">
            {buildRows(config).map((row) => (
              <li key={row.label} className="flex items-start justify-between gap-6 px-5 py-4">
                <div className="flex-1">
                  <p className="text-xs font-medium text-foreground">{row.label}</p>
                  <p className="mt-0.5 text-2xs text-muted-foreground">{row.description}</p>
                </div>
                <span className="shrink-0 font-mono text-xs text-foreground">{row.value}</span>
              </li>
            ))}
          </ul>
        )}
      </Card>
    </>
  );
}

// ──────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────

function ErrorBanner({ children }: { children: React.ReactNode }) {
  return (
    <div className="rounded-md border border-danger/30 bg-danger/5 p-3 text-xs text-danger">
      {children}
    </div>
  );
}

function EmptyHint({ children }: { children: React.ReactNode }) {
  return (
    <div className="rounded-lg border border-dashed border-border-darker px-5 py-8 text-center text-2xs text-muted-foreground">
      {children}
    </div>
  );
}

function formatDate(iso: string) {
  return new Date(iso).toLocaleDateString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
  });
}
