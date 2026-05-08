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
  type ExternalKey,
  type RouterConfig,
} from "@/lib/api";
import { cn } from "@/lib/cn";
import { ChevronDown, Copy, Trash2 } from "lucide-react";
import { useRouter, useSearchParams } from "next/navigation";
import { Suspense, useEffect, useState } from "react";

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

  function setTab(next: TabId) {
    const sp = new URLSearchParams(params.toString());
    sp.set("tab", next);
    router.replace(`/settings?${sp.toString()}`, { scroll: false });
  }

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
      subheader={
        <div className="flex w-full max-w-content-width items-center gap-1 px-3">
          {TABS.map(t => (
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
      }
    >
      <Page.Section>
        {tab === "router-keys" && <RouterKeysPanel />}
        {tab === "provider-keys" && <ProviderKeysPanel />}
        {tab === "config" && <ConfigPanel />}
      </Page.Section>
    </Page>
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
      .then(r => setKeys(r.keys ?? []))
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
            Key created — copy it now, it won&apos;t be shown again.
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

      <Card>
        <Card.Header>
          <Card.Title variant="h4">Issue a new key</Card.Title>
        </Card.Header>
        <Card.Content>
          <form onSubmit={handleCreate} className="flex items-end gap-3">
            <div className="flex-1">
              <Input
                label="Name (optional)"
                placeholder="My API key"
                value={name}
                onChange={e => setName(e.target.value)}
              />
            </div>
            <Button
              type="submit"
              appearance={Appearance.Filled}
              intent={Intent.Primary}
              disabled={creating}
            >
              {creating ? "Creating…" : "Create key"}
            </Button>
          </form>
        </Card.Content>
      </Card>

      {keys.length > 0 ? (
        <Card className="p-0">
          <Card.Header className="border-b border-border px-5 py-3">
            <Card.Title variant="h4">Active router keys</Card.Title>
          </Card.Header>
          <Card.Content>
            <ul className="divide-y divide-border">
              {keys.map(k => (
                <li key={k.id} className="flex items-center justify-between px-5 py-3">
                  <div>
                    <div className="text-xs font-medium text-foreground">
                      {k.name ?? "Unnamed key"}
                    </div>
                    <p className="mt-0.5 font-mono text-2xs text-muted-foreground">
                      {k.key_prefix}…{k.key_suffix}
                      <span className="ml-2 font-sans">
                        · created {formatDate(k.created_at)}
                        {k.last_used_at != null && ` · last used ${formatDate(k.last_used_at)}`}
                      </span>
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

function providerLabel(p: Provider): string {
  return p.charAt(0).toUpperCase() + p.slice(1);
}

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
      .then(r => setKeys(r.keys ?? []))
      .catch((err: unknown) =>
        setError(err instanceof Error ? err.message : "Failed to load keys"),
      );
  }

  useEffect(load, []);

  async function handleSave(e: React.FormEvent) {
    e.preventDefault();
    if (keyValue.trim() === "") return;
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

      <Card>
        <Card.Header>
          <Card.Title variant="h4">Add or replace a key</Card.Title>
        </Card.Header>
        <Card.Content>
          <form onSubmit={handleSave} className="space-y-3">
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-[200px_1fr]">
              <ProviderPicker value={provider} onChange={setProvider} />
              <Input
                label="API key"
                type="password"
                placeholder="sk-..."
                value={keyValue}
                onChange={e => setKeyValue(e.target.value)}
                required
              />
            </div>
            <Input
              label="Name (optional)"
              placeholder="My Anthropic key"
              value={name}
              onChange={e => setName(e.target.value)}
            />
            <div>
              <Button
                type="submit"
                appearance={Appearance.Filled}
                intent={Intent.Primary}
                disabled={saving || keyValue.trim() === ""}
              >
                {saving ? "Saving…" : "Save key"}
              </Button>
            </div>
          </form>
        </Card.Content>
      </Card>

      {keys.length > 0 ? (
        <Card className="p-0">
          <Card.Header className="border-b border-border px-5 py-3">
            <Card.Title variant="h4">Active provider keys</Card.Title>
          </Card.Header>
          <Card.Content>
            <ul className="divide-y divide-border">
              {keys.map(k => (
                <li key={k.id} className="flex items-center justify-between px-5 py-3">
                  <div>
                    <div className="flex items-center gap-2">
                      <span className="text-xs font-medium capitalize text-foreground">
                        {k.provider}
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
}: {
  value: Provider;
  onChange: (p: Provider) => void;
}) {
  const [open, setOpen] = useState(false);
  return (
    <div className="flex flex-col gap-1.5">
      <label className="text-xs font-medium text-foreground">Provider</label>
      <Popover open={open} onOpenChange={setOpen}>
        <Popover.Trigger>
          <Button
            type="button"
            appearance={Appearance.Outlined}
            className="w-full justify-between"
          >
            <span>{providerLabel(value)}</span>
            <ChevronDown className="size-3.5" />
          </Button>
        </Popover.Trigger>
        <Popover.Content className="w-56 p-1" align="start">
          <Command>
            <Command.List>
              {PROVIDERS.map(p => (
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
