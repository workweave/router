"use client";

import { Suspense, useState } from "react";
import Image from "next/image";
import { useRouter, useSearchParams } from "next/navigation";

import { Button } from "@/components/Button";
import { Input } from "@/components/Input";
import { api } from "@/lib/api";

export default function LoginPage() {
  return (
    <Suspense fallback={null}>
      <LoginInner />
    </Suspense>
  );
}

function LoginInner() {
  const router = useRouter();
  const params = useSearchParams();
  const next = params.get("next") || "/dashboard";

  const [password, setPassword] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (!password) return;
    setSubmitting(true);
    setError(null);
    try {
      await api.auth.login(password);
      router.replace(next);
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : "Login failed";
      if (message.includes("503")) {
        setError("Admin login is disabled. Set ROUTER_ADMIN_PASSWORD on the router and restart.");
      } else if (message.includes("401")) {
        setError("Wrong password.");
      } else {
        setError(message);
      }
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <div className="w-full max-w-sm rounded-lg border border-border-darker bg-background p-6 shadow-sm">
      <div className="mb-6 flex items-center gap-3">
        {/* eslint-disable-next-line @next/next/no-img-element */}
        <img src="/ui/weave.svg" alt="Weave" width={32} height={32} className="size-8 rounded-lg" />
        <div>
          <h1 className="font-display text-base font-semibold text-foreground">Weave Router</h1>
          <p className="text-2xs text-muted-foreground">Sign in to the dashboard</p>
        </div>
      </div>

      <form onSubmit={handleSubmit} className="space-y-3">
        <Input
          label="Admin password"
          type="password"
          autoComplete="current-password"
          autoFocus
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          placeholder="ROUTER_ADMIN_PASSWORD"
          required
        />
        {error && (
          <p className="rounded-md border border-danger/30 bg-danger/5 px-3 py-2 text-2xs text-danger">
            {error}
          </p>
        )}
        <Button type="submit" variant="filled" className="w-full" disabled={submitting || !password}>
          {submitting ? "Signing in…" : "Sign in"}
        </Button>
      </form>

      <p className="mt-4 text-2xs text-muted-foreground">
        Set <code className="rounded bg-muted px-1 py-0.5 font-mono">ROUTER_ADMIN_PASSWORD</code> in your
        router&rsquo;s environment to control this password.
      </p>
    </div>
  );
}
