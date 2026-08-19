<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { ArrowLeft, Document, Download, Folder, Picture, Refresh, VideoCamera } from '@element-plus/icons-vue'
import { api } from '../lib/api'

interface StorageItem {
  id: string
  name: string
  type: 'local' | 'webdav' | 'baidu'
  status: string
  statusMessage: string
}

interface BrowserEntry {
  id: string
  name: string
  relativePath: string
  mimeType: string
  size: number
  modifiedAt: string
  isDir: boolean
}

interface BrowserPage {
  currentPath: string
  items: BrowserEntry[] | null
  nextCursor: string
}

const route = useRoute()
const router = useRouter()
const storage = ref<StorageItem>()
const currentPath = ref('')
const entries = ref<BrowserEntry[]>([])
const nextCursor = ref('')
const loading = ref(false)
const errorMessage = ref('')
let requestGeneration = 0

const storageID = computed(() => String(route.params.id))
const breadcrumbs = computed(() => {
  const crumbs = [{ name: storage.value?.name || '存储根目录', path: '' }]
  const segments = currentPath.value.split('/').filter(Boolean)
  let value = ''
  for (const segment of segments) {
    value = value ? `${value}/${segment}` : segment
    crumbs.push({ name: segment, path: value })
  }
  return crumbs
})

async function loadStorage() {
  try {
    storage.value = await api<StorageItem>(`/storage-connections/${storageID.value}`)
  } catch (error) {
    ElMessage.error((error as Error).message)
  }
}

async function load(reset = true) {
  const generation = ++requestGeneration
  loading.value = true
  errorMessage.value = ''
  try {
    const params = new URLSearchParams({ path: currentPath.value, limit: '100' })
    if (!reset && nextCursor.value) params.set('cursor', nextCursor.value)
    const page = await api<BrowserPage>(`/storage-connections/${storageID.value}/objects?${params}`)
    if (generation !== requestGeneration) return
    const pageItems = page.items || []
    entries.value = reset ? pageItems : [...entries.value, ...pageItems]
    nextCursor.value = page.nextCursor || ''
  } catch (error) {
    if (generation !== requestGeneration) return
    entries.value = reset ? [] : entries.value
    errorMessage.value = (error as Error).message
  } finally {
    if (generation === requestGeneration) loading.value = false
  }
}

function navigate(path: string) {
  void router.push({ name: 'storage-browser', params: { id: storageID.value }, query: path ? { path } : {} })
}

function open(entry: BrowserEntry) {
  if (entry.isDir) {
    navigate(entry.relativePath)
    return
  }
  window.open(contentURL(entry), '_blank', 'noopener,noreferrer')
}

function parentPath() {
  const segments = currentPath.value.split('/').filter(Boolean)
  segments.pop()
  navigate(segments.join('/'))
}

function contentURL(entry: BrowserEntry, download = false) {
  const params = new URLSearchParams({ path: entry.relativePath })
  if (download) params.set('download', '1')
  return `/api/v1/storage-connections/${storageID.value}/content?${params}`
}

function iconFor(entry: BrowserEntry) {
  if (entry.isDir) return Folder
  if (entry.mimeType.startsWith('image/')) return Picture
  if (entry.mimeType.startsWith('video/')) return VideoCamera
  return Document
}

function prettySize(size: number, isDir: boolean) {
  if (isDir) return '文件夹'
  if (size < 1024) return `${size} B`
  if (size < 1024 ** 2) return `${(size / 1024).toFixed(1)} KB`
  if (size < 1024 ** 3) return `${(size / 1024 ** 2).toFixed(1)} MB`
  return `${(size / 1024 ** 3).toFixed(1)} GB`
}

function prettyTime(value: string) {
  if (!value || value.startsWith('0001-')) return '—'
  return new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(value))
}

watch(storageID, loadStorage, { immediate: true })
watch(() => route.query.path, value => {
  currentPath.value = typeof value === 'string' ? value : ''
  void load(true)
}, { immediate: true })
</script>

<template>
  <section class="page">
    <header class="page-heading storage-browser-heading">
      <div>
        <router-link to="/storages" class="muted back-link"><el-icon><ArrowLeft /></el-icon>存储管理</router-link>
        <div class="eyebrow">{{ storage?.type || 'Storage' }}</div>
        <h1>{{ storage?.name || '浏览存储' }}</h1>
        <p class="subtitle">查看这个连接中的目录和文件。文件内容通过 ialbum 安全代理。</p>
      </div>
      <el-button :icon="Refresh" :loading="loading" @click="load(true)">刷新</el-button>
    </header>

    <el-alert v-if="storage?.status && storage.status !== 'ready'" type="warning" :closable="false" :title="storage.statusMessage || '当前存储连接状态异常'" style="margin-bottom:18px" />
    <el-alert v-if="errorMessage" type="error" :closable="false" :title="errorMessage" style="margin-bottom:18px" />

    <div class="surface file-browser">
      <div class="file-browser-toolbar">
        <el-button circle plain :icon="ArrowLeft" :disabled="!currentPath" title="返回上级" @click="parentPath" />
        <nav class="breadcrumbs" aria-label="当前目录">
          <template v-for="(crumb,index) in breadcrumbs" :key="crumb.path">
            <span v-if="index" class="breadcrumb-separator">/</span>
            <button :class="{ current: index === breadcrumbs.length - 1 }" @click="navigate(crumb.path)">{{ crumb.name }}</button>
          </template>
        </nav>
      </div>

      <div class="file-table-head" aria-hidden="true"><span>名称</span><span>大小</span><span>修改时间</span><span></span></div>
      <div v-loading="loading && !entries.length" class="file-list">
        <div v-for="entry in entries" :key="`${entry.id}:${entry.relativePath}`" class="file-row">
          <button class="file-name" :title="entry.relativePath" @click="open(entry)">
            <span class="file-icon" :class="{ folder: entry.isDir }"><el-icon><component :is="iconFor(entry)" /></el-icon></span>
            <span>{{ entry.name }}</span>
          </button>
          <span class="file-meta">{{ prettySize(entry.size, entry.isDir) }}</span>
          <span class="file-meta">{{ prettyTime(entry.modifiedAt) }}</span>
          <a v-if="!entry.isDir" class="file-download" :href="contentURL(entry,true)" :download="entry.name" title="下载文件"><el-icon><Download /></el-icon></a>
          <span v-else></span>
        </div>
        <div v-if="!loading && !entries.length && !errorMessage" class="file-empty"><el-icon><Folder /></el-icon><strong>这个目录是空的</strong><span>可以返回上级目录，或在存储端添加文件后刷新。</span></div>
      </div>

      <div v-if="nextCursor" class="file-browser-footer"><el-button :loading="loading" @click="load(false)">加载更多</el-button></div>
    </div>
  </section>
</template>
