<script setup lang="ts">
import { reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { useAuthStore } from '../stores/auth'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()
const loading = ref(false)
const form = reactive({ token: String(route.query.token || ''), username: 'admin', password: '', confirm: '' })
async function submit() {
  if (form.password !== form.confirm) return ElMessage.error('两次输入的密码不一致')
  loading.value = true
  try { await auth.setup(form.token, form.username, form.password); ElMessage.success('管理员账号已创建'); await router.push('/albums') }
  catch (error) { ElMessage.error((error as Error).message) }
  finally { loading.value = false }
}
</script>
<template>
  <div class="auth-page">
    <section class="auth-visual">
      <div class="brand"><span class="brand-mark">i</span><span>ialbum</span></div>
      <div class="auth-copy"><div class="eyebrow" style="color:#b9d8c5">Private by design</div><h1>把珍贵的瞬间，留在自己的空间。</h1><p>ialbum 只索引你选择的存储。照片和视频仍留在原处，由你决定如何备份、浏览与带走。</p></div>
    </section>
    <section class="auth-form-area">
      <form class="auth-card" @submit.prevent="submit">
        <h2>初始化 ialbum</h2><p class="subtitle">使用启动日志中的一次性令牌创建唯一管理员。</p>
        <el-form label-position="top">
          <el-form-item label="初始化令牌"><el-input v-model="form.token" autocomplete="one-time-code" /></el-form-item>
          <el-form-item label="管理员用户名"><el-input v-model="form.username" autocomplete="username" /></el-form-item>
          <el-form-item label="密码"><el-input v-model="form.password" type="password" show-password autocomplete="new-password" placeholder="至少 12 个字符" /></el-form-item>
          <el-form-item label="确认密码"><el-input v-model="form.confirm" type="password" show-password autocomplete="new-password" /></el-form-item>
        </el-form>
        <el-button type="primary" size="large" native-type="submit" :loading="loading" style="width:100%">创建管理员并进入</el-button>
      </form>
    </section>
  </div>
</template>
