import { useEffect, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { api } from '../lib/api'
import { Badge, Button, Card, CardBody, CardHeader, Table } from '../components/ui'

const DECISIONS = [
  { value: 'RESOURCE_CONFIRMED', label: '已确认远端资源存在，关闭' },
  { value: 'NO_RESOURCE_CREATED', label: '已确认未创建任何资源，关闭' },
  { value: 'MANUAL_COMPENSATION_COMPLETED', label: '已完成人工补偿，关闭' },
]

export default function OpsDetail() {
  const { id = '' } = useParams()
  const [audit, setAudit] = useState<any[]>([])
  const [decision, setDecision] = useState(DECISIONS[0].value)
  const [reason, setReason] = useState('')
  const [evidence, setEvidence] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  async function reload() {
    try {
      const a = await api.externalAudit(id)
      setAudit(a.entries ?? a.audit ?? [])
    } catch (e: any) { setError(String(e.message ?? e)) }
  }
  useEffect(() => { reload() }, [id])

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true); setError('')
    try {
      await api.resolveExternal(id, { decision, reason, evidence_reference: evidence })
      await reload()
    } catch (e: any) { setError(String(e.message ?? e)) } finally { setBusy(false) }
  }

  return (
    <div className='space-y-4'>
      <div className='flex items-center gap-3'>
        <Link className='text-sm text-muted-foreground hover:text-foreground' to='/ops'>← 返回对账</Link>
        <h1 className='text-xl font-semibold'>外部操作</h1>
      </div>
      <Card>
        <CardHeader><span className='text-sm font-medium'>记录决策</span></CardHeader>
        <CardBody>
          <form className='space-y-3' onSubmit={submit}>
            <label className='block text-sm'>
              <span className='mb-1 block text-muted-foreground'>决策</span>
              <select className='h-9 w-full rounded-md border border-border bg-background px-3 text-sm' value={decision} onChange={(e) => setDecision(e.target.value)}>
                {DECISIONS.map((d) => <option key={d.value} value={d.value}>{d.label}</option>)}
              </select>
            </label>
            <label className='block text-sm'>
              <span className='mb-1 block text-muted-foreground'>原因（必填）</span>
              <input className='h-9 w-full rounded-md border border-border bg-background px-3 text-sm' value={reason} onChange={(e) => setReason(e.target.value)} required />
            </label>
            <label className='block text-sm'>
              <span className='mb-1 block text-muted-foreground'>证据引用（工单/控制台编号，绝不填写令牌）</span>
              <input className='h-9 w-full rounded-md border border-border bg-background px-3 text-sm' value={evidence} onChange={(e) => setEvidence(e.target.value)} />
            </label>
            {error && <p className='text-sm text-destructive'>{error}</p>}
            <Button type='submit' disabled={busy || reason.trim() === ''}>记录决策</Button>
          </form>
        </CardBody>
      </Card>
      <Card>
        <CardHeader><span className='text-sm font-medium'>审计历史</span></CardHeader>
        <CardBody className='p-0'>
          <Table head={['时间', '动作', '操作者', '决策']}>
            {audit.map((e, i) => (
              <tr key={i} className='border-b border-border/60 last:border-0'>
                <td className='px-3 py-2 text-muted-foreground'>{e.at ?? e.created_at ?? '—'}</td>
                <td className='px-3 py-2'>{e.action ?? e.kind ?? '—'}</td>
                <td className='px-3 py-2 text-muted-foreground'>{e.actor ?? '—'}</td>
                <td className='px-3 py-2'>{e.decision ? <Badge tone='ok'>{e.decision}</Badge> : '—'}</td>
              </tr>
            ))}
            {audit.length === 0 && <tr><td colSpan={4} className='px-3 py-6 text-center text-sm text-muted-foreground'>暂无审计历史</td></tr>}
          </Table>
        </CardBody>
      </Card>
    </div>
  )
}
