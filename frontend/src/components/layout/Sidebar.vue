<script setup lang="ts">
import { computed } from 'vue';
import { useRoute, useRouter } from 'vue-router';
import { useAuthStore } from '@/stores/auth.store';
import { useAlertStore } from '@/stores/alert.store';
import {
  LayoutDashboard, MonitorSpeaker, Bell, Bot, Sparkles,
  Factory, ChevronRight, LogOut, Settings, X, Building2,
} from 'lucide-vue-next';

defineProps<{ showClose?: boolean }>()
const emit = defineEmits<{ close: [] }>()

const route = useRoute();
const router = useRouter();
const auth = useAuthStore();
const alertStore = useAlertStore();

const navItems = [
  { to: '/dashboards',  icon: LayoutDashboard,  label: 'Dashboards' },
  { to: '/machines',    icon: MonitorSpeaker,    label: 'Machines' },
  { to: '/alerts',      icon: Bell,              label: 'Alerts', badge: true },
  { to: '/ai',          icon: Bot,               label: 'AI Assistant' },
  // One entry for Ask Data, not one per plant: the page's own DATA bar is built from
  // GET /ai/scopes, so plant #3 appears there by registering. A per-plant shortcut here
  // would have to be added by hand every time — and was, for NJ5, until MHC arrived.
  { to: '/ask/demo',    icon: Sparkles,          label: 'Ask Data' },
];

// Exact-or-child, not a bare prefix: /dashboards must still light up on /dashboards/:id,
// while /ask/demo must NOT light up on a plant scope like /ask/mhc.
const isActive = (path: string) => route.path === path || route.path.startsWith(path + '/');
const alertCount = computed(() => alertStore.openCount);
const memberOrgs = computed(() => auth.organizations.filter(o => o.isMember));

async function onOrgChange(e: Event) {
  await auth.switchOrg((e.target as HTMLSelectElement).value);
  router.push('/dashboards');
}
</script>

<template>
  <aside class="flex flex-col w-64 min-h-screen bg-surface-100 border-r border-white/5 flex-shrink-0">
    <!-- Logo -->
    <div class="flex items-center gap-3 px-5 py-5 border-b border-white/5">
      <div class="w-8 h-8 rounded-lg bg-gradient-to-br from-primary-500 to-accent-cyan flex items-center justify-center flex-shrink-0">
        <Factory class="w-4 h-4 text-white" />
      </div>
      <div class="flex-1 min-w-0">
        <div class="text-sm font-bold text-white tracking-tight">IotVision</div>
        <div class="text-[10px] text-gray-500 uppercase tracking-widest">Industrial AI</div>
      </div>
      <!-- Close button — shown only on mobile overlay -->
      <button
        v-if="showClose"
        class="p-1.5 rounded-lg text-gray-500 hover:text-white hover:bg-surface-200 transition-colors ml-auto"
        aria-label="Close menu"
        @click="emit('close')"
      >
        <X class="w-4 h-4" />
      </button>
    </div>

    <!-- Org switcher (only shown when user has access to multiple orgs) -->
    <div v-if="memberOrgs.length > 1" class="px-3 py-2 border-b border-white/5">
      <div class="flex items-center gap-2 px-2 py-1.5 rounded-lg bg-surface-200/50">
        <Building2 class="w-3.5 h-3.5 text-gray-400 flex-shrink-0" />
        <select
          :value="auth.activeOrgId ?? ''"
          class="flex-1 bg-transparent text-xs text-gray-300 outline-none cursor-pointer"
          @change="onOrgChange"
        >
          <option v-for="org in memberOrgs" :key="org.id" :value="org.id" class="bg-surface-100 text-gray-200">
            {{ org.name }}
          </option>
        </select>
      </div>
    </div>

    <!-- Navigation -->
    <nav class="flex-1 px-3 py-4 space-y-1">
      <RouterLink
        v-for="item in navItems"
        :key="item.to"
        :to="item.to"
        class="nav-link"
        :class="{ active: isActive(item.to) }"
        @click="emit('close')"
      >
        <component :is="item.icon" class="w-4 h-4 flex-shrink-0" />
        <span class="flex-1">{{ item.label }}</span>
        <span
          v-if="item.badge && alertCount > 0"
          class="flex items-center justify-center w-5 h-5 rounded-full bg-red-500 text-white text-[10px] font-bold"
        >
          {{ alertCount > 9 ? '9+' : alertCount }}
        </span>
        <ChevronRight
          v-if="isActive(item.to)"
          class="w-3.5 h-3.5 text-primary-400"
        />
      </RouterLink>
    </nav>

    <!-- Divider -->
    <div class="border-t border-white/5 mx-3" />

    <!-- User area -->
    <div class="px-3 py-4 space-y-1">
      <button class="nav-link w-full">
        <Settings class="w-4 h-4" />
        <span>Settings</span>
      </button>
      <button class="nav-link w-full text-red-400 hover:text-red-300 hover:bg-red-500/10" @click="auth.logout()">
        <LogOut class="w-4 h-4" />
        <span>Sign out</span>
      </button>
    </div>

    <!-- User info -->
    <div class="px-4 py-4 border-t border-white/5">
      <div class="flex items-center gap-3">
        <div class="w-8 h-8 rounded-full bg-gradient-to-br from-primary-500 to-accent-violet flex items-center justify-center text-xs font-bold text-white">
          {{ auth.user?.name?.[0]?.toUpperCase() ?? '?' }}
        </div>
        <div class="flex-1 min-w-0">
          <div class="text-xs font-medium text-white truncate">{{ auth.user?.name }}</div>
          <div class="text-[10px] text-gray-500 uppercase truncate">{{ auth.user?.role }}</div>
        </div>
      </div>
    </div>
  </aside>
</template>
