import { useEffect, useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { api } from '../lib/api'
import { Badge, Button, Card, CardBody, CardHeader, Input, Table } from '../components/ui'

function tone(status: string) {
  if (!status) return 'muted' as const
  if (status.includes('FAILED')) return 'bad' as const
  if (status.includes('SUCCEEDED') || status.includes('COMPLETED')) return 'ok' as const
  if (status.includes('WAITING') || status.includes('RUNNING')) return 'warn' as const
  return 'muted' as const
}

export default function Runs() {
  const [params, setParams] = useSearchParams()
  const [data, setData] = useState<any>(null)
  const [error, setError] = useState('')
  const [status, setStatus] = useState(params.get('status') ?? '')
  const [template, setTemplate] = useState(params.get('template') ?? '')
  const cursor = params.get('cursor') ?? ''

  useEffect(() => {
    const q = new URLSearchParams()
    if (params.get('status')) q.set('status', params.get('status')!)
    if (params.get('template')) q.set('template', params.get('template')!)
    if (cursor) q.set('cursor', cursor)
    setError('')
    api.runs(q.toString()).then(setData).catch((e) => setError(String(e.message ?? e)))
  }, [params])

  function apply() {
    const q = new URLSearchParams()
    if (status) q.set('status', status)
    if (template) q.set('template', template)
    setParams(q)
  }

  const items: any[] = data?.items ?? []
  const next = data?.next_cursor ?? ''
  return (
    <div className='space-y-4'>
      <div className='flex items-center justify-between'>
        <h1 className='text-xl font-semibold'>运行</h1>
        <Link to='/create' className='rounded-md bg-primary px-4 py-2 text-sm text-primary-foreground'>创建运行</Link>
      </div>
      <Card>
        <CardBody className='flex flex-wrap items-end gap-2'>
          <label className='block text-sm'>
            <span className='mb-1 block text-muted-foreground'>状态筛选</span>
            <Input value={status} onChange={(e) => setStatus(e.target.value)} placeholder='FAILED_TERMINAL' />
          </label>
          <label className='block text-sm'>
            <span className='mb-1 block text-muted-foreground'>模板筛选</span>
            <Input value={template} onChange={(e) => setTemplate(e.target.value)} placeholder='sandbox-email-verify-oidc' />
          </label>
          <Button onClick={apply}>应用筛选</Button>
          <Button variant='outline' onClick={() => { setStatus(''); setTemplate(''); setParams(new URLSearchParams()) }}>清除</Button>
          {cursor && <Button variant='ghost' onClick={() => setParams(new URLSearchParams())}>← 第一页</Button>}
        </CardBody>
      </Card>
      {error && <p className='text-sm text-destructive'>{error}</p>}
      <Card>
        <CardHeader>
          <span className='text-sm font-medium'>运行列表</span>
          <span className='text-xs text-muted-foreground'>本页 {items.length} 条</span>
        </CardHeader>
        <CardBody className='p-0'>
          <Table head={['运行', '模板', '模式', '状态', '步骤', '创建时间']}>
            {items.map((r) => (
              <tr key={r.id} className='border-b border-border/60 last:border-0'>
                <td className='px-3 py-2'>
                  <Link className='text-primary hover:underline' to={`/runs/${r.id}`}>{String(r.id).slice(0, 8)}</Link>
                </td>
                <td className='px-3 py-2'>{r.template}</td>
                <td className='px-3 py-2 text-muted-foreground'>{r.mode}</td>
                <td className='px-3 py-2'><Badge tone={tone(r.status)}>{r.status}</Badge></td>
                <td className='px-3 py-2 text-muted-foreground'>{r.current_step ?? '—'}</td>
                <td className='px-3 py-2 text-muted-foreground'>{r.created_at ?? '—'}</td>
              </tr>
            ))}
            {items.length === 0 && (
              <tr><td colSpan={6} className='px-3 py-6 text-center text-sm text-muted-foreground'>暂无运行</td></tr>
            )}
          </Table>
        </CardBody>
        {next && (
          <div className='border-t border-border px-4 py-3 text-right'>
            <Button variant='outline' size='sm' onClick={() => { const q = new URLSearchParams(params); q.set('cursor', next); setParams(q) }}>下一页 →</Button>
          </div>
        )}
      </Card>
    </div>
  )
}
