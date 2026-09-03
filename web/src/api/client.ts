import axios, {
  type AxiosInstance,
  type InternalAxiosRequestConfig,
  type AxiosResponse
} from 'axios'
import { createDiscreteApi } from 'naive-ui'
import { useAppStore } from '@/stores/app'

const { message } = createDiscreteApi(['message'])

// 统一 axios 实例：baseURL 走 /api/v1（开发态由 Vite 代理到控制面 8080）。
const client: AxiosInstance = axios.create({ baseURL: '/api/v1', timeout: 30000 })

// 请求拦截：附带 Bearer 令牌（来自 Pinia store / localStorage）。
client.interceptors.request.use((cfg: InternalAxiosRequestConfig) => {
  const token = useAppStore().token
  if (token) cfg.headers.set('Authorization', `Bearer ${token}`)
  return cfg
})

// 响应拦截：业务信封 {code,message,detail,data,total}。
//   - code !== 0：统一报错提示，detail 打 console。
//   - 成功：返回完整 AxiosResponse，由 api/* 层按接口结构解包（r.data 即信封）。
client.interceptors.response.use(
  (res: AxiosResponse) => {
    const body = res.data
    if (body && typeof body.code === 'number' && body.code !== 0) {
      message.error(body.message || '请求失败')
      console.error('[API]', body.detail)
      return Promise.reject(new Error(body.message || 'request failed'))
    }
    return res
  },
  (err: unknown) => {
    const e = err as { response?: { status?: number; data?: { message?: string } }; message?: string }
    const status = e.response?.status
    const msg = e.response?.data?.message || e.message || '网络错误'
    if (status === 401) {
      message.error('未授权：请检查访问令牌（系统设置中修改）')
    } else if (status === 403) {
      message.error('权限不足')
    } else {
      message.error(msg)
    }
    return Promise.reject(err)
  }
)

export default client
