import { useEffect, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { api } from '../lib/api'
import { Badge, Card, CardBody, CardHeader, Table } from '../components/ui'

export default function AlertsDetail() {
  const { id = '' } = useParams()
  const [data, setData] = useState<any>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    api.outboxItem(id).then(setData).catch((e) => setError(String(e.message ?? e)))
  }, [id])

  if (error) return <p className='text-sm text-destructive'>{error}</p>
  if (!data) return <p className='text-sm text-muted-foreground'>加载中…</p>

  const item = data.item ?? {}
  const deliveries: any[] = data.deliveries ?? []
  return (
    <div className='space-y-4'>
      <div className='flex items-center gap-3'>
        <Link className='text-sm text-muted-foreground hover:text-foreground' to='/alerts'>← 返回告警</Link>
        <h1 className='text-xl font-semibold'>告警 {String(item.id ?? id).slice(0, 12)}</h1>
        <Badge tone={String(item.status).includes('DEAD') ? 'bad' : 'muted'}>{item.status}</Badge>
      </div>
      <Card>
        <CardHeader><span className='text-sm font-medium'>规则与状态</span></CardHeader>
        <CardBody className='grid gap-2 text-sm sm:grid-cols-2'>
          <div><span className='text-muted-foreground'>规则：</span>{item.rule ?? item.rule_name ?? '—'}</div>
          <div><span className='text-muted-foreground'>严重级别：</span>{item.severity ?? '—'}</div>
          <div><span className='text-muted-foreground'>尝试次数：</span>{item.attempts ?? 0}</div>
          <div><span className='text-muted-foreground'>租约到期：</span>{item.lease_expires_at ?? '—'}</div>
          <div className='sm:col-span-2'><span className='text-muted-foreground'>最近错误：</span>{item.last_error ?? '—'}</div>
        </CardBody>
      </Card>
      <Card>
        <CardHeader><span className='text-sm font-medium'>投递历史</span></CardHeader>
        <CardBody className='p-0'>
          <Table head={['时间', '状态', 'HTTP', '错误']}>
            {deliveries.map((d, i) => (
              <tr key={i} className='border-b border-border/60 last:border-0'>
                <td className='px-3 py-2 text-muted-foreground'>{d.started_at ?? d.created_at ?? '—'}</td>
                <td className='px-3 py-2'>{d.status ?? '—'}</td>
                <td className='px-3 py-2 text-muted-foreground'>{d.http_status ?? '—'}</td>
                <td className='px-3 py-2 text-muted-foreground'>{d.error ?? d.last_error ?? '—'}</td>
              </tr>
            ))}
            {deliveries.length === 0 && <tr><td colSpan={4} className='px-3 py-6 text-center text-sm text-muted-foreground'>暂无投递尝试</td></tr>}
          </Table>
        </CardBody>
      </Card>
    </div>
  )
}
