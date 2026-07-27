import { defineStore } from 'pinia';
import { ref, computed } from 'vue';
import { api } from '@/services/api.service';
import type { Dashboard, DashboardWidget, WidgetType, WidgetLayout, WidgetConfig } from '@/types';

export const useDashboardStore = defineStore('dashboards', () => {
  const dashboards = ref<Dashboard[]>([]);
  const currentDashboard = ref<Dashboard | null>(null);
  const loading = ref(false);
  const saving = ref(false);
  const error = ref<string | null>(null);

  const defaultDashboard = computed(() => dashboards.value.find(d => d.isDefault));
  const widgets = computed(() => currentDashboard.value?.widgets ?? []);

  async function fetchDashboards() {
    loading.value = true;
    try {
      dashboards.value = (await api.getDashboards()) ?? [];
    } catch (err) {
      error.value = (err as Error).message;
    } finally {
      loading.value = false;
    }
  }

  async function fetchDashboard(id: string) {
    loading.value = true;
    try {
      const dash = await api.getDashboard(id);
      currentDashboard.value = dash;
      return dash;
    } catch (err) {
      error.value = (err as Error).message;
      throw err;
    } finally {
      loading.value = false;
    }
  }

  async function createDashboard(payload: { name: string; description?: string; tags?: string[] }) {
    const dashboard = await api.createDashboard(payload);
    dashboards.value.unshift(dashboard);
    return dashboard;
  }

  async function updateDashboard(id: string, payload: Partial<Dashboard>) {
    const updated = await api.updateDashboard(id, payload);
    const idx = dashboards.value.findIndex(d => d.id === id);
    if (idx >= 0) dashboards.value[idx] = { ...dashboards.value[idx], ...updated };
    if (currentDashboard.value?.id === id) {
      currentDashboard.value = { ...currentDashboard.value, ...updated };
    }
    return updated;
  }

  async function deleteDashboard(id: string) {
    await api.deleteDashboard(id);
    dashboards.value = dashboards.value.filter(d => d.id !== id);
    if (currentDashboard.value?.id === id) currentDashboard.value = null;
  }

  async function addWidget(payload: {
    machineId?: string;
    widgetType: WidgetType;
    title?: string;
    layout: WidgetLayout;
    config: WidgetConfig;
  }) {
    if (!currentDashboard.value) throw new Error('No dashboard loaded');
    const widget = await api.addWidget(currentDashboard.value.id, payload);
    if (!currentDashboard.value.widgets) currentDashboard.value.widgets = [];
    currentDashboard.value.widgets.push(widget);
    return widget;
  }

  async function updateWidget(widgetId: string, payload: Partial<DashboardWidget>) {
    if (!currentDashboard.value) return;
    const dashId = currentDashboard.value.id;
    await api.updateWidget(dashId, widgetId, payload);
    // Re-fetch to get fresh joined machine data and trigger widget remount,
    // but keep unsaved local layouts (drags are client-buffered until Save).
    const prevLayouts = new Map((currentDashboard.value.widgets ?? []).map(w => [w.id, w.layout]));
    const fresh = await api.getDashboard(dashId);
    (fresh.widgets ?? []).forEach(w => {
      const l = prevLayouts.get(w.id);
      if (l) w.layout = l;
    });
    currentDashboard.value = fresh;
  }

  async function saveLayout(widgetLayouts: Array<{ id: string; layout: WidgetLayout }>) {
    if (!currentDashboard.value) return;
    saving.value = true;
    try {
      await api.bulkUpdateLayout(currentDashboard.value.id, widgetLayouts);
      // Update local state
      const widgetMap = Object.fromEntries(widgetLayouts.map(w => [w.id, w.layout]));
      (currentDashboard.value.widgets ?? []).forEach(w => {
        if (widgetMap[w.id]) w.layout = widgetMap[w.id];
      });
    } finally {
      saving.value = false;
    }
  }

  async function removeWidget(widgetId: string) {
    if (!currentDashboard.value) return;
    await api.deleteWidget(currentDashboard.value.id, widgetId);
    if (currentDashboard.value.widgets) {
      currentDashboard.value.widgets = currentDashboard.value.widgets.filter(w => w.id !== widgetId);
    }
  }

  return {
    dashboards, currentDashboard, loading, saving, error,
    defaultDashboard, widgets,
    fetchDashboards, fetchDashboard, createDashboard, updateDashboard, deleteDashboard,
    addWidget, updateWidget, saveLayout, removeWidget,
  };
});
