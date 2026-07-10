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
import { ChevronDown, Copy, Filter, KeyRound, Network, Plug, RotateCw, Search, Settings as SettingsIcon, SlidersHorizontal, Trash2 } from "lucide-react";
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
              <SlidersHorizontal className="size-4" />
              <Text variant="h4" as="h3">
                Routing priority
              </Text>
            </Page.SectionHeader>
          }
        >
          <Text className="text-xs text-muted-foreground">
            Bias routing toward stronger models or cheaper ones. Every request
            is balanced between the two. Leave as default to let the router
            decide.
          </Text>
          <RoutingPriorityPanel />
        </Page.Section>

        <Page.Section
          className="py-3"
          header={
            <Page.SectionHeader>
              <Network className="size-4" />
              <Text variant="h4" as="h3">
                Provider selection
              </Text>
            </Page.SectionHeader>
          }
        >
          <Text className="text-xs text-muted-foreground">
            Uncheck a provider to never serve requests through it, including
            failover. Models hosted only by unchecked providers become
            unroutable.
          </Text>
          <ProviderSelectionPanel />
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


// Show the search box only once the list is long enough that scanning it by eye
// gets tedious; for a couple of keys a search bar is just noise.
const KEY_SEARCH_THRESHOLD = 5;

// Shown in place of a name for keys created without one; kept in one place so the
// list row and the search haystack stay in sync.
const UNNAMED_KEY_LABEL = "Unnamed key";

// Alphabetical by name, with unnamed keys pushed to the bottom.
function compareKeysByName(a: APIKey, b: APIKey): number {
  if (a.name == null && b.name == null) return 0;
  if (a.name == null) return 1;
  if (b.name == null) return -1;
  return a.name.localeCompare(b.name);
}

// Case-insensitive substring match over what the row actually shows: the label
// ("Unnamed key" when there's no name), the raw prefix/suffix (so the last few
// characters of a token match), and the exact "prefix…suffix" fingerprint that's
// rendered, so pasting the visible fingerprint verbatim also finds it.
function keyMatchesQuery(k: APIKey, query: string): boolean {
  if (query === "") return true;
  const label = k.name ?? UNNAMED_KEY_LABEL;
  const haystack =
    `${label} ${k.key_prefix} ${k.key_suffix} ${k.key_prefix}…${k.key_suffix}`.toLowerCase();
  return haystack.includes(query);
}

function RouterKeysPanel() {
  const [keys, setKeys] = useState<APIKey[]>([]);
  const [query, setQuery] = useState("");
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [name, setName] = useState("");
  const [creating, setCreating] = useState(false);
  const [rotating, setRotating] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<string | null>(null);
  const [newToken, setNewToken] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);

  const hasKey = keys.length > 0;
  const showSearch = keys.length >= KEY_SEARCH_THRESHOLD;
  const normalizedQuery = query.trim().toLowerCase();
  // Only filter while the search box is actually shown; otherwise a stale query
  // from before the list shrank below the threshold would hide keys with no
  // visible input to clear it.
  const activeQuery = showSearch ? normalizedQuery : "";
  const visibleKeys = keys
    .slice()
    .sort(compareKeysByName)
    .filter(k => keyMatchesQuery(k, activeQuery));

  function load() {
    api.keys
      .list()
      .then(r => {
        setKeys(r.keys ?? []);
        setLoaded(true);
      })
      .catch((err: unknown) => {
        setError(err instanceof Error ? err.message : "Failed to load keys.");
        setLoaded(true);
      });
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
      setError(err instanceof Error ? err.message : "Failed to create key.");
    } finally {
      setCreating(false);
    }
  }

  async function handleRotate(id: string) {
    const confirmed = window.confirm(
      "Rotate this API key?\n\nThe current token will stop working immediately. A new token will be shown once.",
    );
    if (!confirmed) return;
    setRotating(id);
    try {
      const res = await api.keys.rotate(id);
      setNewToken(res.token);
      load();
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : "Failed to rotate key.");
    } finally {
      setRotating(null);
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
      setError(err instanceof Error ? err.message : "Failed to delete key.");
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
            Key created. Copy it; it won&apos;t be shown again.
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

      {loaded && (
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
          <Card.Header className="flex-row items-center justify-between gap-3 border-b border-border px-5 py-3">
            <Card.Title variant="h4">Active router keys</Card.Title>
            {showSearch && (
              <div className="relative w-48">
                <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
                <Input
                  type="search"
                  aria-label="Search router keys"
                  placeholder="Search keys"
                  className="h-8 pl-8 text-xs"
                  value={query}
                  onChange={e => setQuery(e.target.value)}
                />
              </div>
            )}
          </Card.Header>
          <Card.Content>
            {visibleKeys.length === 0 ? (
              <div className="px-5 py-8 text-center text-2xs text-muted-foreground">
                No keys match “{query.trim()}”.
              </div>
            ) : (
            <ul className="divide-y divide-border">
              {visibleKeys.map(k => (
                <li key={k.id} className="flex items-center justify-between gap-3 px-5 py-3">
                  <div className="min-w-0 flex-1">
                    <div className="text-xs font-medium text-foreground">
                      {k.name ?? UNNAMED_KEY_LABEL}
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
                      onClick={() => handleRotate(k.id)}
                      disabled={rotating != null || deleting != null}
                    >
                      <RotateCw className="size-3.5" />
                      {rotating === k.id ? "Rotating…" : "Rotate"}
                    </Button>
                    <Button
                      appearance={Appearance.Hollow}
                      intent={Intent.Danger}
                      size="icon"
                      onClick={() => handleDelete(k.id)}
                      disabled={deleting === k.id || rotating != null}
                      title="Revoke key."
                    >
                      <Trash2 className="size-3.5" />
                    </Button>
                  </div>
                </li>
              ))}
            </ul>
            )}
          </Card.Content>
        </Card>
      ) : loaded ? (
        <EmptyHint>No router keys yet.</EmptyHint>
      ) : null}
    </>
  );
}


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
        setError(err instanceof Error ? err.message : "Failed to load keys."),
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
      setError(err instanceof Error ? err.message : "Failed to delete key.");
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
                    title="Unset the env var and restart the router to remove."
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
                    title="Revoke key."
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
        setError(err instanceof Error ? err.message : "Failed to load config."),
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
        setError(err instanceof Error ? err.message : "Failed to load models."),
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
      setError(err instanceof Error ? err.message : "Failed to save excluded models.");
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


function ProviderSelectionPanel() {
  const [available, setAvailable] = useState<string[] | null>(null);
  const [excluded, setExcluded] = useState<Set<string>>(new Set());
  const [savedExcluded, setSavedExcluded] = useState<Set<string>>(new Set());
  const [envOverrideActive, setEnvOverrideActive] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    api.excludedProviders
      .get()
      .then(res => {
        setAvailable(res.available);
        const ex = new Set(res.excluded);
        setExcluded(ex);
        setSavedExcluded(ex);
        setEnvOverrideActive(res.env_override_active);
      })
      .catch((err: unknown) =>
        setError(err instanceof Error ? err.message : "Failed to load providers."),
      );
  }, []);

  const dirty =
    excluded.size !== savedExcluded.size ||
    Array.from(excluded).some(p => !savedExcluded.has(p));

  function toggle(provider: string) {
    if (envOverrideActive) return;
    setExcluded(prev => {
      const next = new Set(prev);
      if (next.has(provider)) next.delete(provider);
      else next.add(provider);
      return next;
    });
  }

  async function save() {
    setSaving(true);
    setError(null);
    try {
      const res = await api.excludedProviders.update(Array.from(excluded));
      const ex = new Set(res.excluded);
      setExcluded(ex);
      setSavedExcluded(ex);
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : "Failed to save excluded providers.");
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

  return (
    <>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      {envOverrideActive && (
        <div className="rounded-md border border-border bg-muted/30 p-3 text-2xs text-muted-foreground">
          Exclusion list is pinned by <code className="font-mono">ROUTER_EXCLUDED_PROVIDERS</code>;
          clear the env var to edit here.
        </div>
      )}
      <Card className="p-0">
        <Card.Content>
          {available.length === 0 ? (
            <EmptyHint>No deployed providers. Check ROUTER_CLUSTER_VERSION and provider keys.</EmptyHint>
          ) : (
            <ul className="space-y-1 px-5 py-3">
              {available.map(p => {
                const isExcluded = excluded.has(p);
                return (
                  <li key={p} className="flex items-center gap-2">
                    <input
                      type="checkbox"
                      id={`provider-${p}`}
                      checked={!isExcluded}
                      onChange={() => toggle(p)}
                      disabled={envOverrideActive}
                      className="size-3.5"
                    />
                    <label
                      htmlFor={`provider-${p}`}
                      className="cursor-pointer font-mono text-xs text-foreground"
                    >
                      {p}
                    </label>
                  </li>
                );
              })}
            </ul>
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


// Routing dials, rendered as proportional weights (sum to 100%) — mirrors the
// metric-weights editor in the main Weave app. Quality is the scorer's Alpha;
// price is the implied remainder.
const ROUTING_DIALS = [
  { key: "quality", label: "Quality" },
  { key: "price", label: "Price" },
] as const;
const DIAL_COLORS = ["bg-primary", "bg-warning"];
const DEFAULT_QUALITY = 70;

// Distributes a changed weight against the others so they always sum to 100,
// using largest-remainder rounding. Carried over from the main app's
// AggregateMetricWeightsSection so both surfaces behave identically.
function adjustWeightsProportionally(
  weights: Record<string, number>,
  changedKey: string,
  newValue: number,
): Record<string, number> {
  const clamped = Math.max(0, Math.min(100, newValue));
  const keys = Object.keys(weights);
  const otherKeys = keys.filter(k => k !== changedKey);

  if (otherKeys.length === 0) {
    return { [changedKey]: 100 };
  }
  if (clamped === 100) {
    const result: Record<string, number> = { [changedKey]: 100 };
    for (const k of otherKeys) {
      result[k] = 0;
    }
    return result;
  }

  const remaining = 100 - clamped;
  const othersSum = otherKeys.reduce((s, k) => s + weights[k], 0);

  const exact: Record<string, number> = {};
  if (othersSum === 0) {
    const each = remaining / otherKeys.length;
    for (const k of otherKeys) {
      exact[k] = each;
    }
  } else {
    for (const k of otherKeys) {
      exact[k] = (weights[k] / othersSum) * remaining;
    }
  }

  const floored: Record<string, number> = {};
  let flooredSum = 0;
  for (const k of otherKeys) {
    floored[k] = Math.floor(exact[k]);
    flooredSum += floored[k];
  }

  let leftover = remaining - flooredSum;
  const remainders = otherKeys
    .map(k => ({ key: k, remainder: exact[k] - floored[k] }))
    .sort((a, b) => b.remainder - a.remainder);
  for (const { key } of remainders) {
    if (leftover <= 0) break;
    floored[key] += 1;
    leftover -= 1;
  }

  const result: Record<string, number> = { [changedKey]: clamped };
  for (const k of otherKeys) {
    result[k] = floored[k];
  }
  return result;
}

function DistributionBar({ weights }: { weights: Record<string, number> }) {
  return (
    <div className="flex h-2 overflow-hidden rounded-full">
      {ROUTING_DIALS.map((dial, i) => (
        <div
          key={dial.key}
          className={`${DIAL_COLORS[i % DIAL_COLORS.length]} transition-all duration-200`}
          style={{ width: `${weights[dial.key] ?? 0}%` }}
        />
      ))}
    </div>
  );
}

function RoutingPriorityPanel() {
  const [weights, setWeights] = useState<Record<string, number>>({
    quality: DEFAULT_QUALITY,
    price: 100 - DEFAULT_QUALITY,
  });
  const [saved, setSaved] = useState<Record<string, number>>({
    quality: DEFAULT_QUALITY,
    price: 100 - DEFAULT_QUALITY,
  });
  const [focusedKey, setFocusedKey] = useState<string | null>(null);
  const [focusedValue, setFocusedValue] = useState("");
  const [isDefault, setIsDefault] = useState(true);
  const [loaded, setLoaded] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  function apply(res: { quality: number; is_default: boolean }) {
    const quality = Math.round(res.quality);
    const next = { quality, price: 100 - quality };
    setWeights(next);
    setSaved(next);
    setFocusedKey(null);
    setIsDefault(res.is_default);
  }

  useEffect(() => {
    api.routingPreferences
      .get()
      .then(res => {
        apply(res);
        setLoaded(true);
      })
      .catch((err: unknown) => {
        setError(err instanceof Error ? err.message : "Failed to load routing priority");
        setLoaded(true);
      });
  }, []);

  const dirty = weights.quality !== saved.quality;

  function applyProportionalAdjustment(key: string, rawValue: string) {
    const val = parseInt(rawValue, 10);
    if (isNaN(val)) {
      setFocusedKey(null);
      return;
    }
    setWeights(prev => adjustWeightsProportionally(prev, key, val));
    setFocusedKey(null);
  }

  async function save() {
    setSaving(true);
    setError(null);
    try {
      apply(await api.routingPreferences.update({ quality: weights.quality, price: weights.price }));
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : "Failed to save");
    } finally {
      setSaving(false);
    }
  }

  async function reset() {
    setSaving(true);
    setError(null);
    try {
      apply(await api.routingPreferences.reset());
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : "Failed to reset");
    } finally {
      setSaving(false);
    }
  }

  if (!loaded) {
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
      <Card className="p-0">
        <Card.Content className="flex flex-col gap-4 p-5">
          {isDefault && (
            <div className="rounded-md border border-border bg-muted/30 p-3 text-2xs text-muted-foreground">
              Using the router&apos;s default routing. Adjust a weight and save to set a preference.
            </div>
          )}

          <DistributionBar weights={weights} />

          {ROUTING_DIALS.map((dial, i) => (
            <div key={dial.key} className="flex items-center justify-between gap-2">
              <div className="flex items-center gap-1.5">
                <div className={`size-2 rounded-full ${DIAL_COLORS[i % DIAL_COLORS.length]}`} />
                <span className="text-sm text-foreground">{dial.label}</span>
              </div>
              <div className="flex items-center gap-1">
                <Input
                  type="number"
                  min={0}
                  max={100}
                  step={1}
                  className="w-16 text-right text-sm"
                  disabled={saving}
                  value={focusedKey === dial.key ? focusedValue : (weights[dial.key] ?? 0)}
                  onFocus={() => {
                    setFocusedKey(dial.key);
                    setFocusedValue(String(weights[dial.key] ?? 0));
                  }}
                  onChange={e => {
                    const newVal = e.target.value;
                    setFocusedValue(newVal);
                    const parsed = parseInt(newVal, 10);
                    const current = weights[dial.key] ?? 0;
                    if (!isNaN(parsed) && Math.abs(parsed - current) === 1) {
                      setWeights(prev => {
                        const adjusted = adjustWeightsProportionally(prev, dial.key, parsed);
                        setFocusedValue(String(adjusted[dial.key] ?? parsed));
                        return adjusted;
                      });
                    }
                  }}
                  onBlur={() => {
                    if (focusedKey === dial.key) {
                      applyProportionalAdjustment(dial.key, focusedValue);
                    }
                  }}
                  onKeyDown={e => {
                    if (e.key === "ArrowUp" || e.key === "ArrowDown") {
                      e.preventDefault();
                      const delta = e.key === "ArrowUp" ? 1 : -1;
                      setWeights(prev => {
                        const current = prev[dial.key] ?? 0;
                        const next = Math.max(0, Math.min(100, current + delta));
                        const adjusted = adjustWeightsProportionally(prev, dial.key, next);
                        setFocusedValue(String(adjusted[dial.key] ?? next));
                        return adjusted;
                      });
                    }
                    if (e.key === "Enter") {
                      applyProportionalAdjustment(dial.key, focusedValue);
                      (e.target as HTMLInputElement).blur();
                    }
                  }}
                />
                <span className="text-xs text-muted-foreground">%</span>
              </div>
            </div>
          ))}
        </Card.Content>
        <Card.Footer className="flex items-center justify-between border-t border-border px-5 py-3">
          <span className="text-2xs text-muted-foreground">
            Higher quality favors stronger models; higher price favors cheaper ones.
          </span>
          <div className="flex items-center gap-2">
            {!isDefault && (
              <Button appearance={Appearance.Outlined} onClick={reset} disabled={saving}>
                Reset to default
              </Button>
            )}
            <Button
              onClick={save}
              disabled={!dirty || saving}
              intent={Intent.Primary}
              appearance={Appearance.Filled}
            >
              {saving ? "Saving…" : "Save"}
            </Button>
          </div>
        </Card.Footer>
      </Card>
    </>
  );
}

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
