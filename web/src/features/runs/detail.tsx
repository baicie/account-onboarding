import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { api } from '@/lib/api'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'

export function RunDetailPage({ runId }: { runId: string }) {
  const qc = useQueryClient()
  const run = useQuery({ queryKey: ['run', runId], queryFn: () => api.run(runId) })
  const events = useQuery({ queryKey: ['run', runId, 'events'], queryFn: () => api.events(runId) })
  const act = useMutation({
    mutationFn: (kind: 'resume' | 'cancel') =>
      kind === 'resume' ? api.resume(runId) : api.cancel(runId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['run', runId] })
      qc.invalidateQueries({ queryKey: ['runs'] })
    },
  })

  const r = run.data?.run ?? run.data
  const steps: any[] = r?.steps ?? []
  const list: any[] = events.data?.items ?? []

  return (
    <div className='flex flex-col gap-4'>
      <div className='flex items-center justify-between gap-3'>
        <div className='flex items-center gap-3'>
          <Button asChild variant='ghost' size='sm'>
            <Link to='/'>← 全部运行</Link>
          </Button>
          <h1 className='text-2xl font-bold tracking-tight'>运行 {runId.slice(0, 8)}</h1>
          {r?.status && <Badge variant='outline'>{r.status}</Badge>}
        </div>
        <div className='flex gap-2'>
          <Button variant='outline' disabled={act.isPending} onClick={() => act.mutate('resume')}>恢复运行</Button>
          <Button variant='destructive' disabled={act.isPending} onClick={() => act.mutate('cancel')}>取消运行</Button>
        </div>
      </div>

      {act.error && <p className='text-destructive text-sm'>{(act.error as Error).message}</p>}

      <Card>
        <CardHeader className='pb-3'><CardTitle className='text-sm font-medium'>步骤</CardTitle></CardHeader>
        <CardContent className='p-0'>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>步骤</TableHead><TableHead>状态</TableHead><TableHead>尝试</TableHead><TableHead>错误</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {steps.map((s: any, i: number) => (
                <TableRow key={i}>
                  <TableCell>{s.name}</TableCell>
                  <TableCell><Badge variant={String(s.status).includes('FAIL') ? 'destructive' : 'secondary'}>{s.status}</Badge></TableCell>
                  <TableCell className='text-muted-foreground'>{s.attempt_count ?? '—'}</TableCell>
                  <TableCell className='text-muted-foreground'>{s.error_class ?? '—'}</TableCell>
                </TableRow>
              ))}
              {steps.length === 0 && (
                <TableRow><TableCell colSpan={4} className='text-muted-foreground py-8 text-center text-sm'>暂无步骤</TableCell></TableRow>
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      <Card>
        <CardHeader className='pb-3'><CardTitle className='text-sm font-medium'>事件时间线</CardTitle></CardHeader>
        <CardContent className='p-0'>
          <Table>
            <TableHeader>
              <TableRow><TableHead>#</TableHead><TableHead>类型</TableHead><TableHead>时间</TableHead></TableRow>
            </TableHeader>
            <TableBody>
              {list.map((ev: any, i: number) => (
                <TableRow key={i}>
                  <TableCell className='text-muted-foreground'>{ev.seq ?? i + 1}</TableCell>
                  <TableCell>{ev.type ?? ev.kind}</TableCell>
                  <TableCell className='text-muted-foreground'>{ev.at ?? ev.created_at ?? '—'}</TableCell>
                </TableRow>
              ))}
              {list.length === 0 && (
                <TableRow><TableCell colSpan={3} className='text-muted-foreground py-8 text-center text-sm'>暂无事件</TableCell></TableRow>
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>
    </div>
  )
}
