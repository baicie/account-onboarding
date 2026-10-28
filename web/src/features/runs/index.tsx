import { useQuery } from '@tanstack/react-query'
import { Link, useSearch } from '@tanstack/react-router'
import { useState } from 'react'
import { api, type Run } from '@/lib/api'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'

function tone(status: string) {
  if (status.includes('FAILED')) return 'destructive' as const
  if (status.includes('SUCCEEDED') || status.includes('COMPLETED')) return 'secondary' as const
  if (status.includes('WAIT') || status.includes('RUNNING')) return 'outline' as const
  return 'outline' as const
}

export function RunsPage() {
  const search = useSearch({ strict: false }) as { status?: string; template?: string; cursor?: string }
  const [status, setStatus] = useState(search.status ?? '')
  const [template, setTemplate] = useState(search.template ?? '')
  const cursor = search.cursor ?? ''

  const query = new URLSearchParams()
  if (search.status) query.set('status', search.status)
  if (search.template) query.set('template', search.template)
  if (cursor) query.set('cursor', cursor)

  const { data, isLoading, error } = useQuery({
    queryKey: ['runs', query.toString()],
    queryFn: () => api.runs(query.toString()),
  })

  const items: Run[] = data?.items ?? []
  return (
    <div className='flex flex-col gap-4'>
      <div className='flex items-center justify-between'>
        <div>
          <h1 className='text-2xl font-bold tracking-tight'>运行</h1>
          <p className='text-muted-foreground text-sm'>账号开通的每一次编排都有一条运行记录。</p>
        </div>
        <Button asChild>
          <Link to='/runs/new'>创建运行</Link>
        </Button>
      </div>

      <Card>
        <CardHeader className='pb-3'>
          <CardTitle className='text-sm font-medium'>筛选</CardTitle>
        </CardHeader>
        <CardContent className='flex flex-wrap items-end gap-2'>
          <Input className='max-w-56' placeholder='状态，例如 FAILED_TERMINAL' value={status} onChange={(e) => setStatus(e.target.value)} />
          <Input className='max-w-64' placeholder='模板，例如 sandbox-email-verify-oidc' value={template} onChange={(e) => setTemplate(e.target.value)} />
          <Button asChild variant='secondary'>
            <Link to='/' search={{ status: status || undefined, template: template || undefined }}>应用</Link>
          </Button>
          <Button asChild variant='ghost'>
            <Link to='/'>清除</Link>
          </Button>
        </CardContent>
      </Card>

      <Card>
        <CardContent className='p-0'>
          {isLoading && <p className='text-muted-foreground p-4 text-sm'>加载中…</p>}
          {error && <p className='text-destructive p-4 text-sm'>{String((error as Error).message)}</p>}
          {!isLoading && !error && (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>运行</TableHead>
                  <TableHead>模板</TableHead>
                  <TableHead>模式</TableHead>
                  <TableHead>状态</TableHead>
                  <TableHead>步骤</TableHead>
                  <TableHead>创建时间</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {items.map((run) => (
                  <TableRow key={run.id}>
                    <TableCell>
                      <Link className='font-medium underline-offset-4 hover:underline' to='/runs/$runId' params={{ runId: run.id }}>
                        {run.id.slice(0, 8)}
                      </Link>
                    </TableCell>
                    <TableCell className='text-muted-foreground'>{run.template}</TableCell>
                    <TableCell className='text-muted-foreground'>{run.mode}</TableCell>
                    <TableCell>
                      <Badge variant={tone(run.status)}>{run.status}</Badge>
                    </TableCell>
                    <TableCell className='text-muted-foreground'>{run.current_step ?? '—'}</TableCell>
                    <TableCell className='text-muted-foreground'>{run.created_at ?? '—'}</TableCell>
                  </TableRow>
                ))}
                {items.length === 0 && (
                  <TableRow>
                    <TableCell colSpan={6} className='text-muted-foreground py-8 text-center text-sm'>
                      暂无运行
                    </TableCell>
                  </TableRow>
                )}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {data?.next_cursor && (
        <div className='flex justify-end'>
          <Button asChild variant='outline' size='sm'>
            <Link to='/' search={{ ...search, cursor: data.next_cursor }}>下一页 →</Link>
          </Button>
        </div>
      )}
    </div>
  )
}
