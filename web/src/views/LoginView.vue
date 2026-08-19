<script setup lang="ts">
import { reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { useAuthStore } from '../stores/auth'
const router=useRouter();const auth=useAuthStore();const loading=ref(false);const form=reactive({username:'',password:''})
async function submit(){loading.value=true;try{await auth.login(form.username,form.password);await router.push('/albums')}catch(error){ElMessage.error((error as Error).message)}finally{loading.value=false}}
</script>
<template><div class="auth-page"><section class="auth-visual"><div class="brand"><span class="brand-mark">i</span><span>ialbum</span></div><div class="auth-copy"><div class="eyebrow" style="color:#b9d8c5">Your library, your rules</div><h1>欢迎回到<br>你的影像档案。</h1><p>一个安静、清晰的地方，用来整理照片、视频与每一份可靠备份。</p></div></section><section class="auth-form-area"><form class="auth-card" @submit.prevent="submit"><h2>登录</h2><p class="subtitle">请输入管理员账号继续访问私人相册。</p><el-form label-position="top"><el-form-item label="用户名"><el-input v-model="form.username" autocomplete="username" autofocus /></el-form-item><el-form-item label="密码"><el-input v-model="form.password" type="password" show-password autocomplete="current-password" /></el-form-item></el-form><el-button type="primary" size="large" native-type="submit" :loading="loading" style="width:100%">进入 ialbum</el-button></form></section></div></template>
