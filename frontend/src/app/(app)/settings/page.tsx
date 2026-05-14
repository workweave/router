"use client";

import { Text } from "@/components/atoms/Text";
import { Input } from "@/components/Input";
import { Button } from "@/components/molecules/Button";
import { Card } from "@/components/molecules/Card";
import { Command } from "@/components/molecules/Command";
import { Popover } from "@/components/molecules/Popover";
import { Page } from "@/components/Page";
import { PageHeader } from "@/components/PageHeader";
import { Appearance, Intent } from "@/components/types";
import {
  api,
  type APIKey,
  type DeployedModel,
  type ExternalKey,
  type RouterConfig,
} from "@/lib/api";
import { ChevronDown, Copy, Filter, KeyRound, Plug, RotateCw, Scale, Settings as SettingsIcon, Trash2 } from "lucide-react";
import { useEffect, useState } from "react";

export default function SettingsPage() {
  return (
    <Page
      header={
        <PageHeader
          left={
            <Text
              variant="h4"
              as="h2"
              className="flex flex-row items-center gap-1 whitespace-nowrap"
            >
              Settings
            </Text>
          }
        />
      }
    >
      <div className="flex w-full max-w-text-width flex-col gap-2">
        <Page.Section
          className="py-3"
          header={
            <Page.SectionHeader>
              <KeyRound className="size-4" />
              <Text variant="h4" as="h3">
                Router API keys
              </Text>
            </Page.SectionHeader>
          }
        >
          <Text className="text-xs text-muted-foreground">
            Keys used to authenticate requests to this router.
          </Text>
          <RouterKeysPanel />
        </Page.Section>

        <Page.Section
          className="py-3"
          header={
            <Page.SectionHeader>
              <Plug className="size-4" />
              <Text variant="h4" as="h3">
                Provider API keys
              </Text>
            </Page.SectionHeader>
          }
        >
          <Text className="text-xs text-muted-foreground">
            Bring your own keys for Anthropic, OpenAI, Google, OpenRouter.
          </Text>
          <ProviderKeysPanel />
        </Page.Section>

        <Page.Section
          className="py-3"
          header={
            <Page.SectionHeader>
              <Scale className="size-4" />
              <Text variant="h4" as="h3">
                Quality vs cost
              </Text>
            </Page.SectionHeader>
          }
        >
          <Text className="text-xs text-muted-foreground">
            Bias routing toward cheaper models or higher-scoring models in
            0.1 steps. Changes take effect on the next request.
          </Text>
          <RoutingAlphaPanel />
        </Page.Section>

        <Page.Section
          className="py-3"
          header={
            <Page.SectionHeader>
              <Filter className="size-4" />
              <Text variant="h4" as="h3">
                Model selection
              </Text>
            </Page.SectionHeader>
          }
        >
          <Text className="text-xs text-muted-foreground">
            Uncheck a model to drop it from routing decisions for this
            installation. Unchecked models are skipped at request time.
          </Text>
          <ModelSelectionPanel />
        </Page.Section>

        <Page.Section
          className="py-3"
          header={
            <Page.SectionHeader>
              <SettingsIcon className="size-4" />
              <Text variant="h4" as="h3">
                Configuration
              </Text>
            </Page.SectionHeader>
          }
        >
          <Text className="text-xs text-muted-foreground">
            Runtime values set via environment variables.
          </Text>
          <ConfigPanel />
        </Page.Section>
      </div>
    </Page>
  );
}

// ──────────────────────────────────────────────────────────────────────────
// Router keys panel
// ──────────────────────────────────────────────────────────────────────────

function RouterKeysPanel() {
  const [keys, setKeys] = useState<APIKey[]>([]);
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [name, setName] = useState("");
  const [creating, setCreating] = useState(false);
  const [rotating, setRotating] = useState(false);
  const [deleting, setDeleting] = useState<string | null>(null);
  const [newToken, setNewToken] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);

  // hasKey is only meaningful once the list has loaded. Without the loaded
  // gate, the "Issue a new key" form would flash on every page load even
  // when an active key exists, implying multi-key support that the
  // installation_id-scoped partial unique index now forbids.
  const hasKey = keys.length > 0;

  function load() {
    api.keys
      .list()
      .then(r => {
        setKeys(r.keys ?? []);
        setLoaded(true);
      })
      .catch((err: unknown) => {
        setError(err instanceof Error ? err.message : "Failed to load keys");
        setLoaded(true);
      });
  }

  useEffect(load, []);

  async function handleCreate(e: React.FormEvent) {
    e.preventDefault();
    if (hasKey) return; // Belt-and-suspenders against a stale-render submit.
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

  async function handleRotate() {
    if (!hasKey) return;
    const confirmed = window.confirm(
      "Rotate this API key?\n\nThe current token will stop working immediately. A new token will be shown once.",
    );
    if (!confirmed) return;
    setRotating(true);
    try {
      const res = await api.keys.rotate();
      setNewToken(res.token);
      load();
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : "Failed to rotate key");
    } finally {
      setRotating(false);
    }
  }

  async function handleDelete(id: string) {
    const confirmed = window.confirm(
      "Revoke this API key?\n\nThe token will stop working immediately. You will need to issue a new key before clients can authenticate again.",
    );
    if (!confirmed) return;
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
    if (newToken == null) return;
    navigator.clipboard.writeText(newToken).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    });
  }

  return (
    <>
      {error && <ErrorBanner>{error}</ErrorBanner>}

      {newToken != null && (
        <div className="rounded-lg border border-success/30 bg-success/5 p-4">
          <Text className="mb-2 text-xs font-medium text-success">
            Key created. Copy it now, it won&apos;t be shown again.
          </Text>
          <div className="flex items-center gap-2">
            <code className="flex-1 rounded bg-muted px-3 py-1.5 font-mono text-2xs text-foreground">
              {newToken}
            </code>
            <Button appearance={Appearance.Outlined} size="sm" onClick={handleCopy}>
              <Copy className="size-3.5" />
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

      {loaded && !hasKey && (
        <Card>
          <Card.Header>
            <Card.Title variant="h4">Issue a new key</Card.Title>
          </Card.Header>
          <Card.Content>
            <form onSubmit={handleCreate} className="flex items-end gap-3" autoComplete="off">
              <div className="flex-1">
                <Input
                  label="Name (optional)"
                  name="router-key-label"
                  autoComplete="off"
                  data-1p-ignore
                  data-lpignore="true"
                  data-form-type="other"
                  placeholder="My API key"
                  value={name}
                  onChange={e => setName(e.target.value)}
                />
              </div>
              <Button
                type="submit"
                appearance={Appearance.Filled}
                intent={Intent.Primary}
                className="!border-brand !bg-brand !text-white hover:!bg-brand/90"
                disabled={creating}
              >
                {creating ? "Creating…" : "Create key"}
              </Button>
            </form>
          </Card.Content>
        </Card>
      )}

      {hasKey ? (
        <Card className="p-0">
          <Card.Header className="border-b border-border px-5 py-3">
            <Card.Title variant="h4">Active router key</Card.Title>
          </Card.Header>
          <Card.Content>
            <ul className="divide-y divide-border">
              {keys.map(k => (
                <li key={k.id} className="flex items-center justify-between gap-3 px-5 py-3">
                  <div className="min-w-0 flex-1">
                    <div className="text-xs font-medium text-foreground">
                      {k.name ?? "Unnamed key"}
                    </div>
                    <p className="mt-0.5 truncate font-mono text-2xs text-muted-foreground">
                      {k.key_prefix}…{k.key_suffix}
                      <span className="ml-2 font-sans">
                        · created {formatDate(k.created_at)}
                        {k.last_used_at != null && ` · last used ${formatDate(k.last_used_at)}`}
                      </span>
                    </p>
                  </div>
                  <div className="flex items-center gap-2">
                    <Button
                      appearance={Appearance.Outlined}
                      size="sm"
                      onClick={handleRotate}
                      disabled={rotating || deleting != null}
                    >
                      <RotateCw className="size-3.5" />
                      {rotating ? "Rotating…" : "Rotate"}
                    </Button>
                    <Button
                      appearance={Appearance.Hollow}
                      intent={Intent.Danger}
                      size="icon"
                      onClick={() => handleDelete(k.id)}
                      disabled={deleting === k.id || rotating}
                      title="Revoke key"
                    >
                      <Trash2 className="size-3.5" />
                    </Button>
                  </div>
                </li>
              ))}
            </ul>
          </Card.Content>
        </Card>
      ) : loaded ? (
        <EmptyHint>No router keys yet.</EmptyHint>
      ) : null}
    </>
  );
}

// ──────────────────────────────────────────────────────────────────────────
// Provider keys panel
// ──────────────────────────────────────────────────────────────────────────

const PROVIDERS = ["anthropic", "openai", "google", "openrouter"] as const;
type Provider = (typeof PROVIDERS)[number];

const PROVIDER_LABEL: Record<Provider, string> = {
  anthropic: "Anthropic",
  openai: "OpenAI",
  google: "Google",
  openrouter: "OpenRouter",
};

const PROVIDER_ENV_VAR: Record<Provider, string> = {
  anthropic: "ANTHROPIC_API_KEY",
  openai: "OPENAI_API_KEY",
  google: "GOOGLE_API_KEY",
  openrouter: "OPENROUTER_API_KEY",
};

function providerLabel(p: Provider): string {
  return PROVIDER_LABEL[p];
}

function ProviderKeysPanel() {
  const [keys, setKeys] = useState<ExternalKey[]>([]);
  const [envKeyed, setEnvKeyed] = useState<Provider[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [pickedProvider, setPickedProvider] = useState<Provider | null>(null);
  const [keyValue, setKeyValue] = useState("");
  const [name, setName] = useState("");
  const [saving, setSaving] = useState(false);
  const [deleting, setDeleting] = useState<string | null>(null);

  function load() {
    api.providerKeys
      .list()
      .then(r => setKeys(r.keys ?? []))
      .catch((err: unknown) =>
        setError(err instanceof Error ? err.message : "Failed to load keys"),
      );
  }

  useEffect(load, []);

  useEffect(() => {
    api.config
      .get()
      .then(cfg => {
        const set = (cfg.env_provider_keys ?? []).filter((p): p is Provider =>
          (PROVIDERS as readonly string[]).includes(p),
        );
        setEnvKeyed(set);
      })
      .catch(() => {
        // Non-fatal: the panel still works, env-keyed providers just won't
        // be flagged as read-only.
        setEnvKeyed([]);
      });
  }, []);

  const taken = new Set<string>([...keys.map(k => k.provider), ...envKeyed]);
  const available: Provider[] = PROVIDERS.filter(p => !taken.has(p));
  const provider: Provider | null =
    pickedProvider != null && available.includes(pickedProvider)
      ? pickedProvider
      : (available[0] ?? null);

  async function handleSave(e: React.FormEvent) {
    e.preventDefault();
    if (provider == null || keyValue.trim() === "") return;
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

  const hasAnyKey = keys.length > 0 || envKeyed.length > 0;
  return (
    <>
      {error && <ErrorBanner>{error}</ErrorBanner>}

      {available.length > 0 && provider != null ? (
        <Card>
          <Card.Header>
            <Card.Title variant="h4">Add a key</Card.Title>
          </Card.Header>
          <Card.Content>
            <form onSubmit={handleSave} className="space-y-3" autoComplete="off">
              <div className="grid grid-cols-1 gap-3 sm:grid-cols-[200px_1fr]">
                <ProviderPicker value={provider} onChange={setPickedProvider} options={available} />
                <Input
                  label="API key"
                  type="password"
                  name="provider-api-key"
                  autoComplete="new-password"
                  data-1p-ignore
                  data-lpignore="true"
                  data-form-type="other"
                  placeholder="sk-..."
                  value={keyValue}
                  onChange={e => setKeyValue(e.target.value)}
                  required
                />
              </div>
              <Input
                label="Name (optional)"
                name="provider-key-label"
                autoComplete="off"
                data-1p-ignore
                data-lpignore="true"
                data-form-type="other"
                placeholder="My Anthropic key"
                value={name}
                onChange={e => setName(e.target.value)}
              />
              <div>
                <Button
                  type="submit"
                  appearance={Appearance.Filled}
                  intent={Intent.Primary}
                  className="!border-brand !bg-brand !text-white hover:!bg-brand/90"
                  disabled={saving || keyValue.trim() === ""}
                >
                  {saving ? "Saving…" : "Save key"}
                </Button>
              </div>
            </form>
          </Card.Content>
        </Card>
      ) : null}

      {hasAnyKey ? (
        <Card className="p-0">
          <Card.Header className="border-b border-border px-5 py-3">
            <Card.Title variant="h4">Active provider keys</Card.Title>
          </Card.Header>
          <Card.Content>
            <ul className="divide-y divide-border">
              {envKeyed.map(p => (
                <li
                  key={`env-${p}`}
                  className="flex items-center justify-between px-5 py-3"
                >
                  <div>
                    <div className="flex items-center gap-2">
                      <span className="text-xs font-medium text-foreground">
                        {PROVIDER_LABEL[p]}
                      </span>
                      <span className="rounded border border-border bg-muted px-1.5 py-0.5 text-[10px] uppercase tracking-wide text-muted-foreground">
                        env
                      </span>
                    </div>
                    <p className="mt-0.5 font-mono text-2xs text-muted-foreground">
                      Set via {PROVIDER_ENV_VAR[p]}
                    </p>
                  </div>
                  <Button
                    appearance={Appearance.Hollow}
                    intent={Intent.Danger}
                    size="icon"
                    disabled
                    title="Unset the env var and restart the router to remove"
                  >
                    <Trash2 className="size-3.5" />
                  </Button>
                </li>
              ))}
              {keys.map(k => (
                <li key={k.id} className="flex items-center justify-between px-5 py-3">
                  <div>
                    <div className="flex items-center gap-2">
                      <span className="text-xs font-medium text-foreground">
                        {PROVIDER_LABEL[k.provider as Provider] ?? k.provider}
                      </span>
                      {k.name != null && (
                        <span className="text-2xs text-muted-foreground">· {k.name}</span>
                      )}
                    </div>
                    <p className="mt-0.5 font-mono text-2xs text-muted-foreground">
                      {k.key_prefix}…{k.key_suffix}
                    </p>
                  </div>
                  <Button
                    appearance={Appearance.Hollow}
                    intent={Intent.Danger}
                    size="icon"
                    onClick={() => handleDelete(k.id)}
                    disabled={deleting === k.id}
                  >
                    <Trash2 className="size-3.5" />
                  </Button>
                </li>
              ))}
            </ul>
          </Card.Content>
        </Card>
      ) : (
        <EmptyHint>No provider keys configured.</EmptyHint>
      )}
    </>
  );
}

function ProviderPicker({
  value,
  onChange,
  options,
}: {
  value: Provider;
  onChange: (p: Provider) => void;
  options: readonly Provider[];
}) {
  const [open, setOpen] = useState(false);
  return (
    <div className="flex flex-col gap-1.5">
      <label htmlFor="provider-picker" className="text-xs font-medium text-foreground">
        Provider
      </label>
      <Popover open={open} onOpenChange={setOpen}>
        <Popover.Trigger>
          <Button
            id="provider-picker"
            type="button"
            appearance={Appearance.Outlined}
            className="h-9 w-full justify-between border-input px-3 text-sm font-normal"
          >
            <span>{providerLabel(value)}</span>
            <ChevronDown className="size-3.5 text-muted-foreground" />
          </Button>
        </Popover.Trigger>
        <Popover.Content className="w-56 p-1" align="start">
          <Command>
            <Command.List>
              {options.map(p => (
                <Command.Item
                  key={p}
                  onSelect={() => {
                    onChange(p);
                    setOpen(false);
                  }}
                >
                  {providerLabel(p)}
                </Command.Item>
              ))}
            </Command.List>
          </Command>
        </Popover.Content>
      </Popover>
    </div>
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
      label: "Embed only user message",
      value: cfg.embed_only_user_message ? "Enabled" : "Disabled",
      description: "Whether the router embeds user-role text only (no system, assistant, or tool_result content) for cluster routing",
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

      <Card className="p-0">
        <Card.Header className="border-b border-border px-5 py-3">
          <Card.Title variant="h4">Runtime values</Card.Title>
        </Card.Header>
        <Card.Content>
          {config == null ? (
            <div className="px-5 py-8 text-center text-2xs text-muted-foreground">
              {error != null ? "Failed to load" : "Loading…"}
            </div>
          ) : (
            <ul className="divide-y divide-border">
              {buildRows(config).map(row => (
                <li
                  key={row.label}
                  className="flex items-start justify-between gap-6 px-5 py-4"
                >
                  <div className="flex-1">
                    <p className="text-xs font-medium text-foreground">{row.label}</p>
                    <p className="mt-0.5 text-2xs text-muted-foreground">{row.description}</p>
                  </div>
                  <span className="shrink-0 font-mono text-xs text-foreground">{row.value}</span>
                </li>
              ))}
            </ul>
          )}
        </Card.Content>
      </Card>
    </>
  );
}

// ──────────────────────────────────────────────────────────────────────────
// Model selection panel
// ──────────────────────────────────────────────────────────────────────────

function ModelSelectionPanel() {
  const [available, setAvailable] = useState<DeployedModel[] | null>(null);
  const [excluded, setExcluded] = useState<Set<string>>(new Set());
  const [savedExcluded, setSavedExcluded] = useState<Set<string>>(new Set());
  const [envOverrideActive, setEnvOverrideActive] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    api.excludedModels
      .get()
      .then(res => {
        setAvailable(res.available);
        const ex = new Set(res.excluded);
        setExcluded(ex);
        setSavedExcluded(ex);
        setEnvOverrideActive(res.env_override_active);
      })
      .catch((err: unknown) =>
        setError(err instanceof Error ? err.message : "Failed to load models"),
      );
  }, []);

  const dirty =
    excluded.size !== savedExcluded.size ||
    Array.from(excluded).some(m => !savedExcluded.has(m));

  function toggle(model: string) {
    if (envOverrideActive) return;
    setExcluded(prev => {
      const next = new Set(prev);
      if (next.has(model)) next.delete(model);
      else next.add(model);
      return next;
    });
  }

  async function save() {
    setSaving(true);
    setError(null);
    try {
      const res = await api.excludedModels.update(Array.from(excluded));
      const ex = new Set(res.excluded);
      setExcluded(ex);
      setSavedExcluded(ex);
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : "Failed to save");
    } finally {
      setSaving(false);
    }
  }

  if (available == null) {
    return (
      <Card className="p-0">
        <Card.Content>
          <div className="px-5 py-8 text-center text-2xs text-muted-foreground">
            {error != null ? "Failed to load" : "Loading…"}
          </div>
        </Card.Content>
      </Card>
    );
  }

  const grouped = new Map<string, DeployedModel[]>();
  for (const m of available) {
    const arr = grouped.get(m.provider) ?? [];
    arr.push(m);
    grouped.set(m.provider, arr);
  }

  return (
    <>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      {envOverrideActive && (
        <div className="rounded-md border border-border bg-muted/30 p-3 text-2xs text-muted-foreground">
          Exclusion list is pinned by <code className="font-mono">ROUTER_EXCLUDED_MODELS</code>;
          clear the env var to edit here.
        </div>
      )}
      <Card className="p-0">
        <Card.Content>
          {available.length === 0 ? (
            <EmptyHint>No deployed models. Check ROUTER_CLUSTER_VERSION and provider keys.</EmptyHint>
          ) : (
            <div className="divide-y divide-border">
              {Array.from(grouped.entries()).map(([provider, models]) => (
                <div key={provider} className="px-5 py-3">
                  <p className="mb-2 text-2xs font-medium uppercase tracking-wide text-muted-foreground">
                    {provider}
                  </p>
                  <ul className="space-y-1">
                    {models.map(m => {
                      const isExcluded = excluded.has(m.model);
                      return (
                        <li key={m.model} className="flex items-center gap-2">
                          <input
                            type="checkbox"
                            id={`model-${m.model}`}
                            checked={!isExcluded}
                            onChange={() => toggle(m.model)}
                            disabled={envOverrideActive}
                            className="size-3.5"
                          />
                          <label
                            htmlFor={`model-${m.model}`}
                            className="cursor-pointer font-mono text-xs text-foreground"
                          >
                            {m.model}
                          </label>
                        </li>
                      );
                    })}
                  </ul>
                </div>
              ))}
            </div>
          )}
        </Card.Content>
        <Card.Footer className="border-t border-border px-5 py-3">
          <Button
            onClick={save}
            disabled={!dirty || saving || envOverrideActive}
            intent={Intent.Primary}
            appearance={Appearance.Filled}
          >
            {saving ? "Saving…" : "Save"}
          </Button>
        </Card.Footer>
      </Card>
    </>
  );
}

// ──────────────────────────────────────────────────────────────────────────
// Routing alpha panel
// ──────────────────────────────────────────────────────────────────────────

function formatAlpha(value: number, max: number): string {
  // Range is 0..max (default 10) representing α=0.0..1.0. Render the float
  // form so users see the canonical alpha; we never persist or display the
  // raw integer to keep the UI consistent with the paper notation.
  return (value / max).toFixed(1);
}

function RoutingAlphaPanel() {
  const [bounds, setBounds] = useState<{ min: number; max: number } | null>(null);
  const [value, setValue] = useState<number | null>(null);
  const [savedValue, setSavedValue] = useState<number | null>(null);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    api.routingAlpha
      .get()
      .then(res => {
        setBounds({ min: res.min, max: res.max });
        setValue(res.alpha);
        setSavedValue(res.alpha);
      })
      .catch((err: unknown) =>
        setError(err instanceof Error ? err.message : "Failed to load routing alpha"),
      );
  }, []);

  async function commit(next: number) {
    if (savedValue === next) return;
    setSaving(true);
    setError(null);
    try {
      const res = await api.routingAlpha.update(next);
      setValue(res.alpha);
      setSavedValue(res.alpha);
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : "Failed to save routing alpha");
      // Roll back the optimistic slider position on failure so the UI
      // doesn't lie about persisted state.
      setValue(savedValue);
    } finally {
      setSaving(false);
    }
  }

  if (bounds == null || value == null) {
    return (
      <Card className="p-0">
        <Card.Content>
          <div className="px-5 py-8 text-center text-2xs text-muted-foreground">
            {error != null ? "Failed to load" : "Loading…"}
          </div>
        </Card.Content>
      </Card>
    );
  }

  return (
    <>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      <Card>
        <Card.Content>
          <div className="flex flex-col gap-3">
            <div className="flex items-baseline justify-between">
              <Text className="text-2xs uppercase tracking-wide text-muted-foreground">
                Alpha
              </Text>
              <span className="font-mono text-sm text-foreground">
                α = {formatAlpha(value, bounds.max)}
                {saving && <span className="ml-2 text-2xs text-muted-foreground">saving…</span>}
              </span>
            </div>
            <input
              type="range"
              min={bounds.min}
              max={bounds.max}
              step={1}
              value={value}
              onChange={e => setValue(Number(e.target.value))}
              onMouseUp={e => commit(Number((e.target as HTMLInputElement).value))}
              onTouchEnd={e => commit(Number((e.target as HTMLInputElement).value))}
              onKeyUp={e => commit(Number((e.target as HTMLInputElement).value))}
              disabled={saving}
              className="w-full accent-brand"
              aria-label="Routing alpha"
            />
            <div className="flex justify-between text-2xs text-muted-foreground">
              <span>Lowest cost</span>
              <span>Default (0.5)</span>
              <span>Highest quality</span>
            </div>
          </div>
        </Card.Content>
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
    <div className="rounded-lg border-2 border-dashed border-border-darker px-5 py-8 text-center text-2xs text-muted-foreground">
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
