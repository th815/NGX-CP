import { defineStore } from 'pinia'

const TOKEN_KEY = 'ngxcp_token'
const DARK_KEY = 'ngxcp_dark'

// 全局 UI 与访问态：访问令牌（Bearer）、主题。令牌默认开发值，生产应在设置页修改。
export const useAppStore = defineStore('app', {
  state: () => ({
    token: localStorage.getItem(TOKEN_KEY) || 'dev-admin-token',
    dark: localStorage.getItem(DARK_KEY) === '1'
  }),
  actions: {
    setToken(t: string) {
      this.token = t
      localStorage.setItem(TOKEN_KEY, t)
    },
    toggleDark() {
      this.dark = !this.dark
      localStorage.setItem(DARK_KEY, this.dark ? '1' : '0')
    }
  }
})
