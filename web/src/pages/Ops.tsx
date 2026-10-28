import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../lib/api'
import { Badge, Card, CardBody, CardHeader, Table } from '../components/ui'

export default function Ops() {
  const [data, setData] = useState<any>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    api.external().then(setData).catch((e) => setError(String(e.message ?? e)))
  }, [])

  if (error) return <p className='text-sm text-destructive'>{error}</p>
  if (!data) return <p className='text-sm text-muted-foreground'>加载中…</p>

  const rows: any[] = data.operations ?? data.items ?? []
  return (
    <div className='space-y-4'>
      <h1 className='text-xl font-semibold'>外部操作与对账</h1>
      <Card>
        <CardHeader><span className='text-sm font-medium'>外部操作</span></CardHeader>
        <CardBody className='p-0'>
          <Table head={['运行', '服务方', '状态', '版本', '处置结果']}>
            {rows.map((r, i) => (
              <tr key={i} className='border-b border-border/60 last:border-0'>
                <td className='px-3 py-2 text-muted-foreground'>{String(r.run_id ?? '').slice(0, 8)}</td>
                <td className='px-3 py-2'><Link className='text-primary hover:underline' to={`/ops/${r.id ?? r.operation_key}`}>{r.provider ?? r.operation_key}</Link></td>
                <td className='px-3 py-2'><Badge tone={String(r.status).includes('UNRESOLVED') ? 'warn' : 'ok'}>{r.status}</Badge></td>
                <td className='px-3 py-2 text-muted-foreground'>{r.version ?? '—'}</td>
                <td className='px-3 py-2 text-muted-foreground'>{r.resolution ?? '—'}</td>
              </tr>
            ))}
            {rows.length === 0 && <tr><td colSpan={5} className='px-3 py-6 text-center text-sm text-muted-foreground'>没有匹配的外部操作</td></tr>}
          </Table>
        </CardBody>
      </Card>
    </div>
  )
}
