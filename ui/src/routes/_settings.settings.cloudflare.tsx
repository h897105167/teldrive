import { Button, Input, Label, Spinner, TextField } from "@heroui/react";
import { createFileRoute } from "@tanstack/react-router";
import { useState } from "react";
import { toast } from "sonner";
import { $api } from "@/api/client";
import { userMessage } from "@/api/errors";
import { SettingsPageHeader, SettingsRow, SettingsSection } from "@/components/settings-layout";
import { newIdempotencyKey } from "@/features/shared/idempotency";
import { useAppLocale } from "@/lib/locale";

export const Route = createFileRoute("/_settings/settings/cloudflare")({
  component: CloudflareSettings,
  pendingComponent: () => (
    <div className="flex justify-center py-16">
      <Spinner size="lg" />
    </div>
  ),
});

function CloudflareSettings() {
  const { t } = useAppLocale();
  const [apiToken, setApiToken] = useState("");
  const [showToken, setShowToken] = useState(false);
  const [targetZone, setTargetZone] = useState("");
  const [deployed, setDeployed] = useState<{ workerUrl: string; zoneName: string; accountId: string } | null>(null);

  const probe = $api.useSuspenseQuery("get", "/v1/settings/probe", undefined, { staleTime: 15_000 });

  const gateway = $api.useSuspenseQuery("get", "/v1/settings/gateway", undefined, { staleTime: 15_000 });
  // null = untouched, show the server-side value; a string = user is editing.
  const [thresholdDraft, setThresholdDraft] = useState<string | null>(null);
  const thresholdValue = thresholdDraft ?? String(gateway.data.workerMinBytes);
  const thresholdDirty = thresholdDraft !== null && thresholdValue !== String(gateway.data.workerMinBytes);

  const saveThreshold = $api.useMutation("put", "/v1/settings/gateway", {
    onSuccess: (result) => {
      setThresholdDraft(null);
      toast.success(t("cf.splitSaved"), {
        description: t("cf.splitSavedDesc", { bytes: result.workerMinBytes }),
      });
      void gateway.refetch();
    },
    onError: (error) => {
      toast.error(t("cf.splitSaveFailed"), { description: userMessage(error) });
    },
  });

  // One-click data-plane switch.  The server re-checks that the Worker answers
  // /healthz before accepting "worker", so a paused or deleted Worker is refused
  // instead of silently breaking every download.
  const switchMode = $api.useMutation("put", "/v1/settings/gateway", {
    onSuccess: (result) => {
      toast.success(t("cf.modeSwitched"), {
        description:
          result.mode === "worker" ? t("cf.modeSwitchedWorker") : t("cf.modeSwitchedVps"),
      });
      void gateway.refetch();
      void probe.refetch();
    },
    onError: (error) => {
      const status = (error as { status?: number } | null)?.status;
      toast.error(t("cf.modeSwitchFailed"), {
        description: status === 422 ? t("cf.modeUnreachable") : userMessage(error),
      });
    },
  });

  const activeMode = gateway.data.mode;
  const workerReady = gateway.data.gatewayEnabled;

  const deploy = $api.useMutation("post", "/v1/settings/worker-deploy", {
    onSuccess: async (result) => {
      setDeployed({ workerUrl: result.workerUrl, zoneName: result.zoneName, accountId: result.accountId });
      setApiToken("");
      setShowToken(false);
      toast.success(t("cf.deployed"), {
        description: t("cf.deployedDesc", { url: result.workerUrl }),
      });
      void probe.refetch();
    },
    onError: (error) => {
      toast.error(t("cf.deployFailed"), { description: userMessage(error) });
    },
  });

  return (
    <div className="space-y-6">
      <SettingsPageHeader title={t("cf.title")} description={t("cf.desc")} />

      {/* Current status */}
      <SettingsSection title={t("cf.statusTitle")} description={t("cf.statusDesc")}>
        <SettingsRow label={t("cf.tokenConfigured")} description={t("cf.tokenConfiguredHint")}>
          <StatusPill
            active={probe.data.cloudflareApiTokenConfigured}
            labelYes={t("cf.yes")}
            labelNo={t("cf.no")}
          />
        </SettingsRow>
        <SettingsRow label={t("cf.gatewayEnabled")} description={t("cf.gatewayEnabledHint")}>
          <StatusPill active={probe.data.gatewayEnabled} labelYes={t("cf.yes")} labelNo={t("cf.no")} />
        </SettingsRow>
      </SettingsSection>

      {/* Data plane switch */}
      <SettingsSection title={t("cf.modeTitle")} description={t("cf.modeDesc")}>
        <SettingsRow label={t("cf.modeLabel")} description={t("cf.modeHint")} align="start">
          <div className="space-y-3">
            <div className="flex flex-wrap gap-2">
              <Button
                variant={activeMode === "worker" ? "primary" : "secondary"}
                isDisabled={switchMode.isPending || activeMode === "worker" || !workerReady}
                onPress={() => switchMode.mutateAsync({ body: { mode: "worker" } })}
              >
                {t("cf.modeWorker")}
              </Button>
              <Button
                variant={activeMode === "vps" ? "primary" : "secondary"}
                isDisabled={switchMode.isPending || activeMode === "vps"}
                onPress={() => switchMode.mutateAsync({ body: { mode: "vps" } })}
              >
                {t("cf.modeVps")}
              </Button>
            </div>
            <p className="text-xs text-muted">
              {switchMode.isPending
                ? t("cf.modeSwitching")
                : !workerReady
                  ? t("cf.modeNotConfigured")
                  : `${t("cf.modeCurrent")}: ${
                      activeMode === "worker" ? t("cf.modeWorker") : t("cf.modeVps")
                    } · ${gateway.data.workerUrl || t("cf.workerUrlUnknown")}`}
            </p>
          </div>
        </SettingsRow>
      </SettingsSection>

      {/* Download split threshold */}
      <SettingsSection title={t("cf.splitTitle")} description={t("cf.splitDesc")}>
        <SettingsRow label={t("cf.splitLabel")} description={t("cf.splitHint")} align="start">
          <div className="space-y-2">
            <TextField className="min-w-full">
              <Label className="sr-only">{t("cf.splitLabel")}</Label>
              <Input
                type="number"
                min={0}
                step={1}
                value={thresholdValue}
                onChange={(event) => setThresholdDraft(event.currentTarget.value)}
                autoComplete="off"
                spellCheck={false}
              />
            </TextField>
            <div className="flex justify-end gap-2">
              {thresholdDirty ? (
                <Button
                  variant="ghost"
                  isDisabled={saveThreshold.isPending}
                  onPress={() => setThresholdDraft(null)}
                >
                  {t("cf.splitReset")}
                </Button>
              ) : null}
              <Button
                variant="primary"
                isDisabled={!thresholdDirty || saveThreshold.isPending}
                onPress={() => {
                  const bytes = Number(thresholdValue);
                  if (!Number.isInteger(bytes) || bytes < 0) return;
                  saveThreshold.mutateAsync({ body: { workerMinBytes: bytes } });
                }}
              >
                {saveThreshold.isPending ? t("cf.splitSaving") : t("cf.splitSave")}
              </Button>
            </div>
          </div>
        </SettingsRow>
      </SettingsSection>

      {/* Deploy form */}
      <SettingsSection title={t("cf.deployTitle")} description={t("cf.deployDesc")}>
        <SettingsRow label={t("cf.apiTokenLabel")} description={t("cf.apiTokenHint")} align="start">
          <div className="space-y-2">
            <TextField className="min-w-full">
              <Label className="sr-only">{t("cf.apiTokenLabel")}</Label>
              <Input
                type={showToken ? "text" : "password"}
                value={apiToken}
                onChange={(event) => setApiToken(event.currentTarget.value)}
                placeholder="CF..."
                autoComplete="off"
                spellCheck={false}
              />
            </TextField>
            <label className="flex items-center gap-2 text-xs text-muted">
              <input
                type="checkbox"
                checked={showToken}
                onChange={(event) => setShowToken(event.currentTarget.checked)}
                className="size-4 accent-accent"
              />
              {t("cf.showToken")}
            </label>
          </div>
        </SettingsRow>
        <SettingsRow label={t("cf.targetZoneLabel")} description={t("cf.targetZoneHint")} align="start">
          <TextField className="min-w-full">
            <Label className="sr-only">{t("cf.targetZoneLabel")}</Label>
            <Input
              type="text"
              value={targetZone}
              onChange={(event) => setTargetZone(event.currentTarget.value)}
              placeholder="example.com"
              autoComplete="off"
              spellCheck={false}
            />
          </TextField>
        </SettingsRow>
        <SettingsRow label="" description="">
          <div className="flex justify-end">
            <Button
              variant="primary"
              isDisabled={!apiToken.trim() || deploy.isPending}
              onPress={() => {
                if (!apiToken.trim()) return;
              deploy.mutateAsync({
                  params: { header: { "Idempotency-Key": newIdempotencyKey() } },
                  body: {
                    cloudflareApiToken: apiToken.trim(),
                    ...(targetZone.trim() ? { targetZone: targetZone.trim() } : {}),
                  },
                });
              }}
            >
              {deploy.isPending ? t("cf.deploying") : t("cf.deployBtn")}
            </Button>
          </div>
        </SettingsRow>
      </SettingsSection>

      {/* Success banner */}
      {deployed ? (
        <div className="rounded-xl border border-success/30 bg-success/10 px-5 py-4">
          <p className="text-sm font-semibold text-success">{t("cf.deployed")}</p>
          <div className="mt-2 space-y-0.5 text-sm">
            <p>
              <span className="text-muted">{t("cf.workerUrl")}:</span>{" "}
              <a
                href={deployed.workerUrl}
                target="_blank"
                rel="noopener noreferrer"
                className="font-mono text-accent hover:underline"
              >
                {deployed.workerUrl}
              </a>
            </p>
            <p className="font-mono text-xs text-muted">
              {deployed.zoneName} · {deployed.accountId}
            </p>
          </div>
        </div>
      ) : null}
    </div>
  );
}

function StatusPill({ active, labelYes, labelNo }: { active: boolean; labelYes: string; labelNo: string }) {
  return (
    <span
      className={`inline-flex items-center gap-1.5 rounded-full px-3 py-1 text-xs font-medium ${
        active
          ? "bg-success/15 text-success"
          : "bg-default-200 text-default-600 dark:bg-default-100 dark:text-default-500"
      }`}
    >
      <span
        className={`size-1.5 rounded-full ${active ? "bg-success" : "bg-default-500"}`}
        aria-hidden
      />
      {active ? labelYes : labelNo}
    </span>
  );
}
