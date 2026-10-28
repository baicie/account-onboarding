import { useEffect, useState } from 'react'
import { api } from '../lib/api'
import { Badge, Button, Card, CardBody, CardHeader, Table } from '../components/ui'

export default function Alerts() {
  const [data, setData] = useState<any>(null)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  async function reload() {
    try { setData(await api.outbox()) } catch (e: any) { setError(String(e.message ?? e)) }
  }
  useEffect(() => { reload() }, [])

  async function dispatch() {
    setBusy(true)
    try { await api.dispatch(); await reload() } catch (e: any) { setError(String(e.message ?? e)) } finally { setBusy(false) }
  }

  if (error) return <p className='text-sm text-destructive'>{error}</p>
  if (!data) return <p className='text-sm text-muted-foreground'>加载中…</p>

  const rows: any[] = data.rows ?? data.items ?? []
  return (
    <div className='space-y-4'>
      <div className='flex items-center justify-between'>
        <h1 className='text-xl font-semibold'>告警与发件箱</h1>
        <Button variant='outline' disabled={busy} onClick={dispatch}>立即投递</Button>
      </div>
      <Card>
        <CardHeader><span className='text-sm font-medium'>发件箱记录</span></CardHeader>
        <CardBody className='p-0'>
          <Table head={['规则', '严重级别', '状态', '尝试', '最近错误']}>
            {rows.map((r, i) => (
              <tr key={i} className='border-b border-border/60 last:border-0'>
                <td className='px-3 py-2'>{r.rule ?? r.rule_name}</td>
                <td className='px-3 py-2 text-muted-foreground'>{r.severity ?? '—'}</td>
                <td className='px-3 py-2'><Badge tone={String(r.status).includes('DEAD') ? 'bad' : 'muted'}>{r.status}</Badge></td>
                <td className='px-3 py-2 text-muted-foreground'>{r.attempts ?? 0}</td>
                <td className='px-3 py-2 text-muted-foreground'>{r.last_error ?? '—'}</td>
              </tr>
            ))}
            {rows.length === 0 && <tr><td colSpan={5} className='px-3 py-6 text-center text-sm text-muted-foreground'>没有匹配的发件箱记录</td></tr>}
          </Table>
        </CardBody>
      </Card>
    </div>
  )
}
