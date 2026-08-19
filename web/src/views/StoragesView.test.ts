// @vitest-environment jsdom
import { flushPromises, mount } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import StoragesView from './StoragesView.vue'

const apiMock = vi.fn()

vi.mock('../lib/api', () => ({
  api: (...args: unknown[]) => apiMock(...args),
  jsonBody: (value: unknown) => JSON.stringify(value),
}))

describe('StoragesView', () => {
  beforeEach(() => apiMock.mockReset())

  it('renders the empty state when there are no connections', async () => {
    // Keep the page resilient to older binaries that returned null for an
    // empty Go slice. Newer APIs consistently return [].
    apiMock.mockResolvedValue(null)
    const wrapper = mount(StoragesView, { global: { plugins: [ElementPlus] } })
    await flushPromises()
    expect(wrapper.text()).toContain('存储管理')
    expect(wrapper.text()).toContain('还没有存储连接')
  })
})
