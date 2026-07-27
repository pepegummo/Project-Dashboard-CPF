import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router';
import { useAuthStore } from '@/stores/auth.store';

const routes: RouteRecordRaw[] = [
  {
    path: '/login',
    name: 'Login',
    component: () => import('@/pages/LoginPage.vue'),
    meta: { public: true, layout: 'blank' },
  },
  {
    // LED kiosk page — no auth required, no sidebar/topbar
    // Access via: /led?w=<base64-encoded LedWidget[]>
    // Generate the URL with useLedExport → exportLedLink()
    path: '/led',
    name: 'LedView',
    component: () => import('@/pages/LedViewPage.vue'),
    meta: { public: true, layout: 'blank' },
  },
  {
    path: '/',
    redirect: '/dashboards',
    meta: { requiresAuth: true },
  },
  {
    path: '/dashboards',
    name: 'DashboardList',
    component: () => import('@/pages/DashboardListPage.vue'),
    meta: { requiresAuth: true, title: 'Dashboards' },
  },
  {
    path: '/dashboards/:id',
    name: 'DashboardEditor',
    component: () => import('@/pages/DashboardEditorPage.vue'),
    meta: { requiresAuth: true, title: 'Dashboard Editor' },
  },
  {
    path: '/machines',
    name: 'MachineManagement',
    component: () => import('@/pages/MachineManagementPage.vue'),
    meta: { requiresAuth: true, title: 'Machines' },
  },
  {
    path: '/alerts',
    name: 'Alerts',
    component: () => import('@/pages/AlertsPage.vue'),
    meta: { requiresAuth: true, title: 'Alerts' },
  },
  {
    path: '/ai',
    name: 'AIAssistant',
    component: () => import('@/pages/AIAssistantPage.vue'),
    meta: { requiresAuth: true, title: 'AI Assistant' },
  },
  {
    path: '/ask',
    redirect: '/ask/demo',
  },
  {
    // One URL shape for every dataset: /ask/demo is the mock telemetry, /ask/<slug>
    // is one real plant (factories.slug, owned by the source registry, e.g. /ask/nj5).
    // A factory with no slug is addressed by its uuid. The page resolves the param
    // against GET /ai/scopes, so standing up plant #2 needs no change here.
    path: '/ask/:scope',
    name: 'AskData',
    component: () => import('@/pages/AskDataPage.vue'),
    meta: { requiresAuth: true, title: 'Ask Data' },
  },
  {
    path: '/:pathMatch(.*)*',
    redirect: '/dashboards',
  },
];

const router = createRouter({
  history: createWebHistory(),
  routes,
  scrollBehavior: () => ({ top: 0 }),
});

router.beforeEach((to) => {
  const auth = useAuthStore();
  if (!to.meta.public && !auth.isAuthenticated) {
    return { name: 'Login', query: { redirect: to.fullPath } };
  }
  if (to.name === 'Login' && auth.isAuthenticated) {
    return { name: 'DashboardList' };
  }
});

export default router;
