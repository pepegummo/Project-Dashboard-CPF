import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router';
import { useAuthStore } from '@/stores/auth.store';

// The factory id scripts/nj5-registry.sql seeds. /nj5 is the sidebar entry for it, and
// AskDataPage sends the scope picker back to this route when that plant is chosen, so
// the highlighted nav item always matches the data on screen.
export const NJ5_FACTORY_ID = '00000000-0000-0000-0001-000000004046';

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
    name: 'AskData',
    component: () => import('@/pages/AskDataPage.vue'),
    meta: { requiresAuth: true, title: 'Ask Data' },
  },
  {
    // Same page as /ask, preset to the Nongjok5 factory in the canonical dataset.
    // The factory id is the fixed one scripts/nj5-registry.sql seeds; the page falls
    // back to the first available scope if that factory has no registered sources.
    path: '/nj5',
    name: 'AskNJ5',
    component: () => import('@/pages/AskDataPage.vue'),
    props: { dataset: 'canonical', factoryId: NJ5_FACTORY_ID },
    meta: { requiresAuth: true, title: 'NJ5' },
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
