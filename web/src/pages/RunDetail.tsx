import { useEffect, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { api } from '../lib/api'
import { Badge, Button, Card, CardBody, CardHeader, Table } from '../components/ui'

export default function RunDetail() {
  const { id = '' } = useParams()
  const [run, setRun] = useState<any>(null)
  const [events, setEvents] = useState<any[]>([])
  const [error, setError] = useState('')
  const [busy, setBusy] = useState('')

  async function reload() {
    try {
      const [r, e] = await Promise.all([api.run(id), api.events(id)])
      setRun(r.run ?? r)
      setEvents(e.events ?? [])
    } catch (e: any) {
      setError(String(e.message ?? e))
    }
  }

  useEffect(() => { reload() }, [id])

  async function act(kind: 'resume' | 'cancel') {
    setBusy(kind)
    try {
      kind === 'resume' ? await api.resume(id) : await api.cancel(id)
      await reload()
    } catch (e: any) {
      setError(String(e.message ?? e))
    } finally {
      setBusy('')
    }
  }

  if (error) return <p className='text-sm text-destructive'>{error}</p>
  if (!run) return <p className='text-sm text-muted-foreground'>加载中…</p>

  const steps: any[] = run.steps ?? []
  return (
    <div className='space-y-4'>
      <div className='flex items-center justify-between'>
        <div className='flex items-center gap-3'>
          <Link className='text-sm text-muted-foreground hover:text-foreground' to='/'>← 全部运行</Link>
          <h1 className='text-xl font-semibold'>运行 {String(run.id).slice(0, 8)}</h1>
          <Badge tone='muted'>{run.status}</Badge>
        </div>
        <div className='flex gap-2'>
          <Button variant='outline' disabled={busy !== ''} onClick={() => act('resume')}>恢复运行</Button>
          <Button variant='destructive' disabled={busy !== ''} onClick={() => act('cancel')}>取消运行</Button>
        </div>
      </div>

      <Card>
        <CardHeader><span className='text-sm font-medium'>步骤</span></CardHeader>
        <CardBody className='p-0'>
          <Table head={['步骤', '状态', '尝试', '错误']}>
            {steps.map((s, i) => (
              <tr key={i} className='border-b border-border/60 last:border-0'>
                <td className='px-3 py-2'>{s.name}</td>
                <td className='px-3 py-2'><Badge tone={String(s.status).includes('FAIL') ? 'bad' : 'ok'}>{s.status}</Badge></td>
                <td className='px-3 py-2 text-muted-foreground'>{s.attempts ?? '—'}</td>
                <td className='px-3 py-2 text-muted-foreground'>{s.error_class ?? '—'}</td>
              </tr>
            ))}
            {steps.length === 0 && <tr><td colSpan={4} className='px-3 py-6 text-center text-sm text-muted-foreground'>暂无步骤</td></tr>}
          </Table>
        </CardBody>
      </Card>

      <Card>
        <CardHeader><span className='text-sm font-medium'>事件时间线</span></CardHeader>
        <CardBody className='p-0'>
          <Table head={['#', '类型', '步骤', '时间']}>
            {events.map((ev, i) => (
              <tr key={i} className='border-b border-border/60 last:border-0'>
                <td className='px-3 py-2 text-muted-foreground'>{i + 1}</td>
                <td className='px-3 py-2'>{ev.type}</td>
                <td className='px-3 py-2 text-muted-foreground'>{ev.step ?? '—'}</td>
                <td className='px-3 py-2 text-muted-foreground'>{ev.at ?? ev.created_at ?? '—'}</td>
              </tr>
            ))}
            {events.length === 0 && <tr><td colSpan={4} className='px-3 py-6 text-center text-sm text-muted-foreground'>暂无事件</td></tr>}
          </Table>
        </CardBody>
      </Card>
    </div>
  )
}
