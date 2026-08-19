import { afterEach, describe, expect, it, vi } from 'vitest'
import { api, jsonBody, setCSRFToken } from './api'

describe('api client', () => {
  afterEach(() => { vi.unstubAllGlobals(); setCSRFToken('') })

  it('unwraps data and applies CSRF to mutations', async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ data: { ok: true } }), { status: 200, headers: { 'Content-Type': 'application/json' } }))
    vi.stubGlobal('fetch', fetchMock)
    setCSRFToken('csrf-token')
    await expect(api('/albums', { method: 'POST', body: jsonBody({ name: '旅行' }) })).resolves.toEqual({ ok: true })
    const [, request] = fetchMock.mock.calls[0]
    expect(request.headers.get('X-CSRF-Token')).toBe('csrf-token')
    expect(request.headers.get('Content-Type')).toBe('application/json')
  })

  it('returns the API error message', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(JSON.stringify({ error: { code: 'conflict', message: '目录冲突' } }), { status: 409 })))
    await expect(api('/albums')).rejects.toMatchObject({ message: '目录冲突', status: 409, code: 'conflict' })
  })
})
