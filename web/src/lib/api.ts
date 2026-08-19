export interface APIErrorShape { error: { code: string; message: string; details?: unknown; requestId?: string } }
export interface DataEnvelope<T> { data: T }

let csrfToken = ''
export function setCSRFToken(token: string) { csrfToken = token }
export function getCSRFToken() { return csrfToken }

export async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers)
  if (init.body && !(init.body instanceof FormData)) headers.set('Content-Type', 'application/json')
  if (csrfToken && !['GET', 'HEAD'].includes((init.method || 'GET').toUpperCase())) headers.set('X-CSRF-Token', csrfToken)
  const response = await fetch(`/api/v1${path}`, { ...init, headers, credentials: 'same-origin' })
  if (response.status === 204) return undefined as T
  const body = await response.json().catch(() => ({}))
  if (!response.ok) {
    const message = (body as APIErrorShape).error?.message || `请求失败（${response.status}）`
    const error = new Error(message) as Error & { status?: number; code?: string }
    error.status = response.status
    error.code = (body as APIErrorShape).error?.code
    throw error
  }
  return (body as DataEnvelope<T>).data
}

export function jsonBody(value: unknown): BodyInit { return JSON.stringify(value) }
