import { useEffect, useState } from 'react'
import { api } from '../lib/api'
import { Badge, Button, Card, CardBody, CardHeader, Input, Table } from '../components/ui'

export default function Alerts() {
  const [data, setData] = useState<any>(null)
  const [rule, setRule] = useState('')
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState('')

  async function reload() {
    try { setData(await api.outbox()) } catch (e: any) { setError(String(e.message ?? e)) }
  }
  useEffect(() => { reload() }, [])

  async function run(kind: 'dispatch' | 'rearm' | 'resolve') {
    if (kind !== 'dispatch' && rule.trim() === '') { setError('请填写规则名'); return }
    setBusy(kind); setError(''); setNotice('')
    try {
      const res = kind === 'dispatch' ? await api.dispatch() : kind === 'rearm' ? await api.rearm(rule.trim()) : await api.resolveRule(rule.trim())
      setNotice(kind === 'dispatch' ? `已投递 ${res?.sent ?? 0} 条` : kind === 'rearm' ? `已重新启用 ${res?.rearmed ?? 0} 条终态记录` : `已处置 ${res?.resolved ?? 0} 条活跃记录`)
      await reload()
    } catch (e: any) {
      setError(String(e.message ?? e))
    } finally { setBusy('') }
  }

  if (!data) return <p className='text-sm text-muted-foreground'>加载中…</p>
  const rows: any[] = data.rows ?? data.items ?? []

  return (
    <div className='space-y-4'>
      <h1 className='text-xl font-semibold'>告警与发件箱</h1>
      <Card>
        <CardHeader><span className='text-sm font-medium'>规则操作</span></CardHeader>
        <CardBody className='space-y-3'>
          <div className='flex flex-wrap items-end gap-2'>
            <label className='block text-sm'>
              <span className='mb-1 block text-muted-foreground'>规则名</span>
              <Input value={rule} onChange={(e) => setRule(e.target.value)} placeholder='例如 rule-name' />
            </label>
            <Button variant='outline' disabled={busy !== ''} onClick={() => run('rearm')}>重新启用</Button>
            <Button variant='outline' disabled={busy !== ''} onClick={() => run('resolve')}>处置活跃记录</Button>
            <Button disabled={busy !== ''} onClick={() => run('dispatch')}>立即投递</Button>
          </div>
          {notice && <p className='text-sm text-emerald-400'>{notice}</p>}
          {error && <p className='text-sm text-destructive'>{error}</p>}
        </CardBody>
      </Card>
      <Card>
        <CardHeader>
          <span className='text-sm font-medium'>发件箱记录</span>
          <span className='text-xs text-muted-foreground'>共 {rows.length} 条</span>
        </CardHeader>
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
