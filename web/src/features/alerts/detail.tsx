import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { api } from '@/lib/api'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'

export function AlertDetailPage({ alertId }: { alertId: string }) {
  const { data, isLoading, error } = useQuery({
    queryKey: ['outbox', alertId],
    queryFn: () => api.outboxItem(alertId),
  })
  const item = data?.item ?? {}
  const deliveries: any[] = data?.deliveries ?? []

  return (
    <div className='flex flex-col gap-4'>
      <div className='flex items-center gap-3'>
        <Button asChild variant='ghost' size='sm'><Link to='/alerts'>← 返回告警</Link></Button>
        <h1 className='text-2xl font-bold tracking-tight'>告警 {alertId.slice(0, 12)}</h1>
        {item.status && <Badge variant={String(item.status).includes('DEAD') ? 'destructive' : 'outline'}>{item.status}</Badge>}
      </div>
      {isLoading && <p className='text-muted-foreground text-sm'>加载中…</p>}
      {error && <p className='text-destructive text-sm'>{(error as Error).message}</p>}

      <Card>
        <CardHeader className='pb-3'><CardTitle className='text-sm font-medium'>规则与状态</CardTitle></CardHeader>
        <CardContent className='grid gap-2 text-sm sm:grid-cols-2'>
          <div><span className='text-muted-foreground'>规则：</span>{item.rule ?? item.rule_name ?? '—'}</div>
          <div><span className='text-muted-foreground'>严重级别：</span>{item.severity ?? '—'}</div>
          <div><span className='text-muted-foreground'>尝试次数：</span>{item.attempts ?? 0}</div>
          <div><span className='text-muted-foreground'>租约到期：</span>{item.lease_expires_at ?? '—'}</div>
          <div className='sm:col-span-2'><span className='text-muted-foreground'>最近错误：</span>{item.last_error ?? '—'}</div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader className='pb-3'><CardTitle className='text-sm font-medium'>投递历史</CardTitle></CardHeader>
        <CardContent className='p-0'>
          <Table>
            <TableHeader>
              <TableRow><TableHead>时间</TableHead><TableHead>状态</TableHead><TableHead>HTTP</TableHead><TableHead>错误</TableHead></TableRow>
            </TableHeader>
            <TableBody>
              {deliveries.map((d: any, i: number) => (
                <TableRow key={i}>
                  <TableCell className='text-muted-foreground'>{d.started_at ?? d.created_at ?? '—'}</TableCell>
                  <TableCell>{d.status ?? '—'}</TableCell>
                  <TableCell className='text-muted-foreground'>{d.http_status ?? '—'}</TableCell>
                  <TableCell className='text-muted-foreground'>{d.error ?? d.last_error ?? '—'}</TableCell>
                </TableRow>
              ))}
              {deliveries.length === 0 && (
                <TableRow><TableCell colSpan={4} className='text-muted-foreground py-8 text-center text-sm'>暂无投递尝试</TableCell></TableRow>
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>
    </div>
  )
}
