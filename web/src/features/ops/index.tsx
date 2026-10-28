import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { api } from '@/lib/api'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent } from '@/components/ui/card'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'

export function OpsPage() {
  const { data, isLoading, error } = useQuery({ queryKey: ['external'], queryFn: () => api.external() })
  const rows: any[] = data?.operations ?? data?.items ?? []
  return (
    <div className='flex flex-col gap-4'>
      <div>
        <h1 className='text-2xl font-bold tracking-tight'>外部对账</h1>
        <p className='text-muted-foreground text-sm'>结果未知的外部副作用落在这里，需要人工核对后记录决策。</p>
      </div>
      <Card>
        <CardContent className='p-0'>
          {isLoading && <p className='text-muted-foreground p-4 text-sm'>加载中…</p>}
          {error && <p className='text-destructive p-4 text-sm'>{(error as Error).message}</p>}
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>运行</TableHead><TableHead>服务方</TableHead><TableHead>状态</TableHead><TableHead>版本</TableHead><TableHead>处置结果</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((r: any, i: number) => (
                <TableRow key={i}>
                  <TableCell className='text-muted-foreground'>{String(r.run_id ?? '').slice(0, 8)}</TableCell>
                  <TableCell>
                    <Link className='underline-offset-4 hover:underline' to='/ops/$opId' params={{ opId: r.id ?? r.operation_key }}>
                      {r.provider ?? r.operation_key}
                    </Link>
                  </TableCell>
                  <TableCell><Badge variant={String(r.status).includes('UNRESOLVED') ? 'outline' : 'secondary'}>{r.status}</Badge></TableCell>
                  <TableCell className='text-muted-foreground'>{r.version ?? '—'}</TableCell>
                  <TableCell className='text-muted-foreground'>{r.resolution ?? '—'}</TableCell>
                </TableRow>
              ))}
              {rows.length === 0 && !isLoading && (
                <TableRow><TableCell colSpan={5} className='text-muted-foreground py-8 text-center text-sm'>没有匹配的外部操作</TableCell></TableRow>
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>
    </div>
  )
}
