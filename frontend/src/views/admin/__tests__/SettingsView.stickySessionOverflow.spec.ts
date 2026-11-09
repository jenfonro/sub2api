import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { enableAutoUnmount, flushPromises, shallowMount } from "@vue/test-utils";

import type { UpdateSettingsRequest } from "@/api/admin/settings";
import enSettings from "@/i18n/locales/en/admin/settings";
import zhSettings from "@/i18n/locales/zh/admin/settings";
import SettingsView from "../SettingsView.vue";

const {
  getSettings,
  updateSettings,
  fetchPublicSettings,
  adminSettingsFetch,
  showError,
  localeRef,
} = vi.hoisted(() => ({
  getSettings: vi.fn(),
  updateSettings: vi.fn(),
  fetchPublicSettings: vi.fn().mockResolvedValue(undefined),
  adminSettingsFetch: vi.fn().mockResolvedValue(undefined),
  showError: vi.fn(),
  localeRef: { value: "zh" },
}));

vi.mock("@/api", () => ({
  adminAPI: {
    settings: {
      getSettings,
      updateSettings,
      getWebSearchEmulationConfig: vi.fn().mockResolvedValue({
        enabled: false,
        providers: [],
      }),
      updateWebSearchEmulationConfig: vi.fn().mockResolvedValue({
        enabled: false,
        providers: [],
      }),
      getAdminApiKey: vi.fn().mockResolvedValue({ exists: false, masked_key: "" }),
      getOverloadCooldownSettings: vi.fn().mockResolvedValue({}),
      getRateLimit429CooldownSettings: vi.fn().mockResolvedValue({}),
      getPanelRateLimitSettings: vi.fn().mockResolvedValue({}),
      getStreamTimeoutSettings: vi.fn().mockResolvedValue({}),
      getRectifierSettings: vi.fn().mockResolvedValue({}),
      getBetaPolicySettings: vi.fn().mockResolvedValue({ rules: [] }),
    },
    accounts: {
      getUpstreamBillingProbeSettings: vi.fn().mockResolvedValue({}),
      getOllamaCloudUsageSettings: vi.fn().mockResolvedValue({}),
      getOpenCodeGoUsageSettings: vi.fn().mockResolvedValue({}),
    },
    groups: { getAll: vi.fn().mockResolvedValue([]) },
    proxies: { list: vi.fn().mockResolvedValue({ items: [] }) },
    payment: { getProviders: vi.fn().mockResolvedValue({ data: [] }) },
  },
}));

vi.mock("@/stores", () => ({
  useAppStore: () => ({
    showError,
    showSuccess: vi.fn(),
    fetchPublicSettings,
  }),
}));

vi.mock("@/stores/adminSettings", () => ({
  useAdminSettingsStore: () => ({ fetch: adminSettingsFetch }),
}));

vi.mock("@/composables/useClipboard", () => ({
  useClipboard: () => ({ copyToClipboard: vi.fn() }),
}));

vi.mock("vue-i18n", async () => {
  const actual = await vi.importActual<typeof import("vue-i18n")>("vue-i18n");
  return {
    ...actual,
    useI18n: () => ({
      locale: localeRef,
      t: (key: string) => {
        const prefix = "admin.settings.scheduling.";
        if (!key.startsWith(prefix)) return key;
        const copy = localeRef.value === "zh"
          ? zhSettings.settings.scheduling
          : enSettings.settings.scheduling;
        return copy[key.slice(prefix.length) as keyof typeof copy] ?? key;
      },
    }),
  };
});

enableAutoUnmount(afterEach);

// Omit the new setting to represent an unconfigured server response.
const baseSettingsResponse = {
  backend_mode_enabled: false,
  default_balance: 0,
  default_concurrency: 1,
  table_default_page_size: 20,
  table_page_size_options: [10, 20, 50, 100],
  wechat_connect_open_enabled: false,
  wechat_connect_mp_enabled: false,
  wechat_connect_mobile_enabled: false,
};

const toggleSelector = '[data-testid="openai-sticky-session-overflow-toggle"]';

async function mountGatewaySettings() {
  const wrapper = shallowMount(SettingsView, {
    global: {
      stubs: {
        AppLayout: { template: "<main><slot /></main>" },
        // Exercise the real switch's click handling and aria-checked state.
        Toggle: false,
      },
    },
  });
  await flushPromises();
  await wrapper.get("#settings-tab-gateway").trigger("click");
  return wrapper;
}

describe.each(["zh", "en"])("SettingsView sticky session overflow (%s)", (locale) => {
  beforeEach(() => {
    vi.clearAllMocks();
    localeRef.value = locale;
    getSettings.mockReset().mockResolvedValue({ ...baseSettingsResponse });
    updateSettings.mockReset().mockImplementation(async (payload: UpdateSettingsRequest) => ({
      ...baseSettingsResponse,
      ...payload,
    }));
  });

  it("defaults to the legacy policy when the setting is absent", async () => {
    const wrapper = await mountGatewaySettings();
    const toggle = wrapper.get(toggleSelector);
    const copy = locale === "zh"
      ? zhSettings.settings.scheduling
      : enSettings.settings.scheduling;

    expect(toggle.isVisible()).toBe(true);
    expect(toggle.attributes("role")).toBe("switch");
    expect(toggle.attributes("aria-checked")).toBe("false");
    expect(toggle.attributes("type")).toBe("button");
    expect(toggle.element.closest(".card")?.querySelector("h2")?.textContent?.trim())
      .toBe(copy.title);
    expect(wrapper.get('label[for="openai-sticky-session-overflow-enabled"]').text())
      .toBe(copy.openaiStickySessionOverflow);
    expect(toggle.attributes("aria-describedby"))
      .toBe("openai-sticky-session-overflow-hint openai-sticky-session-overflow-scope");
    expect(wrapper.get("#openai-sticky-session-overflow-hint").text())
      .toBe(copy.openaiStickySessionOverflowHint);
    expect(wrapper.get("#openai-sticky-session-overflow-scope").text())
      .toBe(copy.openaiStickySessionOverflowScopeHint);

    await wrapper.get("form").trigger("submit");
    await flushPromises();

    expect(updateSettings).toHaveBeenCalledOnce();
    expect(updateSettings).toHaveBeenCalledWith(expect.objectContaining({
      openai_sticky_session_overflow_enabled: false,
    }));
    expect(showError).not.toHaveBeenCalled();
  });

  it("loads and preserves an existing enabled setting", async () => {
    getSettings.mockResolvedValueOnce({
      ...baseSettingsResponse,
      openai_sticky_session_overflow_enabled: true,
    });
    const wrapper = await mountGatewaySettings();

    expect(wrapper.get(toggleSelector).attributes("aria-checked")).toBe("true");
    await wrapper.get("form").trigger("submit");
    await flushPromises();

    expect(updateSettings).toHaveBeenCalledOnce();
    expect(updateSettings).toHaveBeenCalledWith(expect.objectContaining({
      openai_sticky_session_overflow_enabled: true,
    }));
    expect(wrapper.get(toggleSelector).attributes("aria-checked")).toBe("true");
    expect(showError).not.toHaveBeenCalled();
  });

  it.each([
    { initial: false, next: true },
    { initial: true, next: false },
  ])("saves $next after switching from $initial", async ({ initial, next }) => {
    getSettings.mockResolvedValueOnce({
      ...baseSettingsResponse,
      openai_sticky_session_overflow_enabled: initial,
    });
    const wrapper = await mountGatewaySettings();
    const toggle = wrapper.get(toggleSelector);

    expect(toggle.attributes("aria-checked")).toBe(String(initial));
    await toggle.trigger("click");
    expect(toggle.attributes("aria-checked")).toBe(String(next));
    expect(updateSettings).not.toHaveBeenCalled();

    await wrapper.get("form").trigger("submit");
    await flushPromises();

    expect(updateSettings).toHaveBeenCalledOnce();
    expect(updateSettings).toHaveBeenCalledWith(expect.objectContaining({
      openai_sticky_session_overflow_enabled: next,
    }));
    expect(toggle.attributes("aria-checked")).toBe(String(next));
    expect(fetchPublicSettings).toHaveBeenCalledWith(true);
    expect(adminSettingsFetch).toHaveBeenCalledWith(true);
    expect(showError).not.toHaveBeenCalled();
  });
});
