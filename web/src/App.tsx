import { useEffect, useState } from 'react'
import { NavLink, Route, Routes, useNavigate } from 'react-router-dom'
import { loadSession, type Session } from './lib/api'
import Runs from './pages/Runs'
import CreateRun from './pages/CreateRun'
import RunDetail from './pages/RunDetail'
import Alerts from './pages/Alerts'
import Ops from './pages/Ops'
import { cx } from './components/ui'

const nav = [
  { to: '/', label: '运行', end: true },
  { to: '/ops', label: '外部对账' },
  { to: '/alerts', label: '告警' },
]

export default function App() {
  const [session, setSession] = useState<Session | null>(null)
  const [error, setError] = useState('')
  const navigate = useNavigate()

  useEffect(() => {
    loadSession().then(setSession).catch((e) => setError(String(e.message ?? e)))
  }, [])

  if (error) {
    return (
      <div className='flex min-h-screen items-center justify-center p-6'>
        <div className='max-w-md text-center'>
          <h1 className='text-lg font-semibold'>会话不可用</h1>
          <p className='mt-2 text-sm text-muted-foreground'>{error}</p>
          <a className='mt-4 inline-block rounded-md bg-primary px-4 py-2 text-sm text-primary-foreground' href='/ui/login'>重新登录</a>
        </div>
      </div>
    )
  }
  if (!session) return <div className='p-6 text-sm text-muted-foreground'>加载中…</div>

  async function logout() {
    await fetch('/ui/logout', { method: 'POST', credentials: 'include', headers: { 'X-CSRF-Token': session?.csrf ?? '' } })
    navigate('/ui/login', { external: true } as never)
    window.location.href = '/ui/login'
  }

  return (
    <div className='flex min-h-screen'>
      <aside className='w-56 shrink-0 border-r border-border bg-card/60 p-4'>
        <div className='mb-6 flex items-center gap-2'>
          <span className='text-sm font-semibold'>账号开通控制台</span>
          <span className='rounded bg-muted px-1.5 py-0.5 text-[10px] text-muted-foreground'>v{session.version}</span>
        </div>
        <nav className='space-y-1'>
          {nav.map((n) => (
            <NavLink
              key={n.to}
              to={n.to}
              end={n.end}
              className={({ isActive }) =>
                cx('block rounded-md px-3 py-2 text-sm', isActive ? 'bg-primary/15 text-primary' : 'text-muted-foreground hover:bg-muted')
              }
            >
              {n.label}
            </NavLink>
          ))}
        </nav>
      </aside>
      <div className='flex min-w-0 flex-1 flex-col'>
        <header className='flex h-14 items-center justify-between border-b border-border px-6'>
          <span className='text-sm text-muted-foreground'>{session.actor}</span>
          <div className='flex items-center gap-3'>
            <span className='text-xs text-muted-foreground'>{session.role}</span>
            <button className='text-sm text-muted-foreground hover:text-foreground' onClick={logout}>退出登录</button>
          </div>
        </header>
        <main className='min-w-0 flex-1 p-6'>
          <Routes>
            <Route path='/' element={<Runs />} />
            <Route path='/create' element={<CreateRun />} />
            <Route path='/runs/:id' element={<RunDetail />} />
            <Route path='/alerts' element={<Alerts />} />
            <Route path='/ops' element={<Ops />} />
          </Routes>
        </main>
      </div>
    </div>
  )
}
