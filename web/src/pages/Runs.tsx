import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../lib/api'
import { Badge, Card, CardBody, CardHeader, Table } from '../components/ui'

function tone(status: string) {
  if (!status) return 'muted' as const
  if (status.includes('FAILED')) return 'bad' as const
  if (status.includes('SUCCEEDED') || status.includes('COMPLETED')) return 'ok' as const
  if (status.includes('WAITING') || status.includes('RUNNING')) return 'warn' as const
  return 'muted' as const
}

export default function Runs() {
  const [data, setData] = useState<any>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    api.runs().then(setData).catch((e) => setError(String(e.message ?? e)))
  }, [])

  if (error) return <p className='text-sm text-destructive'>{error}</p>
  if (!data) return <p className='text-sm text-muted-foreground'>加载中…</p>

  const runs: any[] = data.runs ?? data.items ?? []
  return (
    <div className='space-y-4'>
      <div className='flex items-center justify-between'>
        <h1 className='text-xl font-semibold'>运行</h1>
        <Link to='/create' className='rounded-md bg-primary px-4 py-2 text-sm text-primary-foreground'>创建运行</Link>
      </div>
      <Card>
        <CardHeader>
          <span className='text-sm font-medium'>最近运行</span>
          <span className='text-xs text-muted-foreground'>共 {runs.length} 条</span>
        </CardHeader>
        <CardBody className='p-0'>
          <Table head={['运行', '模板', '模式', '状态', '步骤', '创建时间']}>
            {runs.map((r) => (
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
            {runs.length === 0 && (
              <tr><td colSpan={6} className='px-3 py-6 text-center text-sm text-muted-foreground'>暂无运行</td></tr>
            )}
          </Table>
        </CardBody>
      </Card>
    </div>
  )
}
