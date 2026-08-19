<script setup lang="ts">
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { Collection, Files, Setting, SwitchButton, Timer } from '@element-plus/icons-vue'
import { useAuthStore } from './stores/auth'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()
const showShell = computed(() => auth.user && !['/login', '/setup'].includes(route.path))
async function logout() { await auth.logout(); await router.push('/login') }
</script>

<template>
  <div v-if="showShell" class="app-shell">
    <aside class="sidebar">
      <router-link to="/albums" class="brand" aria-label="ialbum 首页"><span class="brand-mark">i</span><span>ialbum</span></router-link>
      <nav class="nav-list" aria-label="主导航">
        <router-link to="/albums"><el-icon><Collection /></el-icon><span>我的相册</span></router-link>
        <router-link to="/storages"><el-icon><Files /></el-icon><span>存储管理</span></router-link>
        <router-link to="/jobs"><el-icon><Timer /></el-icon><span>任务中心</span></router-link>
        <router-link to="/settings"><el-icon><Setting /></el-icon><span>系统设置</span></router-link>
      </nav>
      <div class="sidebar-footer">
        <div class="user-chip"><span class="avatar">{{ auth.user?.username.slice(0, 1).toUpperCase() }}</span><span>{{ auth.user?.username }}</span></div>
        <button class="icon-button" title="退出登录" @click="logout"><el-icon><SwitchButton /></el-icon></button>
      </div>
    </aside>
    <main class="main-content"><router-view /></main>
  </div>
  <router-view v-else />
</template>
