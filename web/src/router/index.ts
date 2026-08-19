import { createRouter, createWebHistory } from 'vue-router'
import { useAuthStore } from '../stores/auth'

const AlbumsView = () => import('../views/AlbumsView.vue')
const AlbumDetailView = () => import('../views/AlbumDetailView.vue')
const StoragesView = () => import('../views/StoragesView.vue')
const StorageBrowserView = () => import('../views/StorageBrowserView.vue')
const JobsView = () => import('../views/JobsView.vue')
const SettingsView = () => import('../views/SettingsView.vue')
const LoginView = () => import('../views/LoginView.vue')
const SetupView = () => import('../views/SetupView.vue')

const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', redirect: '/albums' },
    { path: '/setup', component: SetupView, meta: { public: true } },
    { path: '/login', component: LoginView, meta: { public: true } },
    { path: '/albums', component: AlbumsView },
    { path: '/albums/:id', component: AlbumDetailView },
    { path: '/storages', component: StoragesView },
    { path: '/storages/:id', name: 'storage-browser', component: StorageBrowserView },
    { path: '/jobs', component: JobsView },
    { path: '/settings', component: SettingsView },
  ],
})

router.beforeEach(async (to) => {
  const auth = useAuthStore()
  await auth.bootstrap()
  if (!auth.setupComplete && to.path !== '/setup') return '/setup'
  if (auth.setupComplete && !auth.user && !to.meta.public) return '/login'
  if (auth.user && ['/login', '/setup'].includes(to.path)) return '/albums'
})

export default router
