import { defineStore } from 'pinia'
import { api, jsonBody, setCSRFToken } from '../lib/api'

interface User { id: string; username: string }
interface SessionPayload { user: User; csrfToken: string; expiresAt: string }

export const useAuthStore = defineStore('auth', {
  state: () => ({ user: null as User | null, setupComplete: true, initialized: false }),
  actions: {
    async bootstrap() {
      if (this.initialized) return
      const status = await api<{ setupComplete: boolean }>('/setup/status')
      this.setupComplete = status.setupComplete
      if (status.setupComplete) {
        try {
          const session = await api<SessionPayload>('/auth/me')
          this.applySession(session)
        } catch { this.user = null }
      }
      this.initialized = true
    },
    applySession(session: SessionPayload) { this.user = session.user; setCSRFToken(session.csrfToken) },
    async login(username: string, password: string) {
      const session = await api<SessionPayload>('/auth/login', { method: 'POST', body: jsonBody({ username, password }) })
      this.applySession(session)
    },
    async setup(token: string, username: string, password: string) {
      await api('/setup', { method: 'POST', body: jsonBody({ token, username, password }) })
      this.setupComplete = true
      await this.login(username, password)
    },
    async logout() { await api('/auth/session', { method: 'DELETE' }); this.user = null; setCSRFToken('') },
  },
})
