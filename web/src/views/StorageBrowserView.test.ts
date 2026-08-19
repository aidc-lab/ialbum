// @vitest-environment jsdom
import { flushPromises, mount } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import { describe, expect, it, vi } from 'vitest'
import StorageBrowserView from './StorageBrowserView.vue'

const apiMock = vi.fn()
const pushMock = vi.fn()

vi.mock('../lib/api', () => ({ api: (...args: unknown[]) => apiMock(...args) }))
vi.mock('vue-router', async importOriginal => {
  const original = await importOriginal<typeof import('vue-router')>()
  return {
    ...original,
    useRoute: () => ({ params: { id: 'storage-1' }, query: {} }),
    useRouter: () => ({ push: pushMock }),
  }
})

describe('StorageBrowserView', () => {
  it('renders an empty directory without crashing', async () => {
    apiMock.mockImplementation((path: string) => path.endsWith('/storage-1')
      ? Promise.resolve({ id: 'storage-1', name: '家庭 NAS', type: 'webdav', status: 'ready', statusMessage: '' })
      : Promise.resolve({ currentPath: '', items: null, nextCursor: '' }))
    const wrapper = mount(StorageBrowserView, {
      global: {
        plugins: [ElementPlus],
        stubs: { RouterLink: { template: '<a><slot /></a>' } },
      },
    })
    await flushPromises()
    expect(wrapper.text()).toContain('家庭 NAS')
    expect(wrapper.text()).toContain('这个目录是空的')
  })
})
