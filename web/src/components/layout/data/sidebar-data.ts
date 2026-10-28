import { LayoutDashboard, Bell, ArrowLeftRight, Command, ShieldCheck } from 'lucide-react'
import { type SidebarData } from '../types'

// Navigation for the operator console: three sections, mirroring the JSON API surface.
export const sidebarData: SidebarData = {
  user: {
    name: 'operator',
    email: 'admin token',
    avatar: '/avatars/shadcn.jpg',
  },
  teams: [
    {
      name: '账号开通控制台',
      logo: Command,
      plan: 'Account Onboarding',
    },
  ],
  navGroups: [
    {
      title: '运维',
      items: [
        { title: '运行', url: '/', icon: LayoutDashboard },
        { title: '外部对账', url: '/ops', icon: ArrowLeftRight },
        { title: '告警', url: '/alerts', icon: Bell },
        { title: '服务方', url: '/providers', icon: ShieldCheck },
      ],
    },
  ],
}
