import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { api } from '@/lib/api'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'

export function AlertsPage() {
  const qc = useQueryClient()
  const [rule, setRule] = useState('')
  const [notice, setNotice] = useState('')
  const outbox = useQuery({ queryKey: ['outbox'], queryFn: () => api.outbox() })
  const act = useMutation({
    mutationFn: async (kind: 'dispatch' | 'rearm' | 'resolve') => {
      if (kind === 'dispatch') return api.dispatch()
      if (rule.trim() === '') throw new Error('请填写规则名')
      return kind === 'rearm' ? api.rearm(rule.trim()) : api.resolveRule(rule.trim())
    },
    onSuccess: (_res, kind) => {
      setNotice(kind === 'dispatch' ? '已触发投递' : kind === 'rearm' ? '已重新启用' : '已处置活跃记录')
      qc.invalidateQueries({ queryKey: ['outbox'] })
    },
  })

  const rows: any[] = outbox.data?.rows ?? outbox.data?.items ?? []
  return (
    <div className='flex flex-col gap-4'>
      <div>
        <h1 className='text-2xl font-bold tracking-tight'>告警与发件箱</h1>
        <p className='text-muted-foreground text-sm'>至少一次投递；崩溃后可能重复，投递历史永不被改写。</p>
      </div>

      <Card>
        <CardHeader className='pb-3'><CardTitle className='text-sm font-medium'>规则操作</CardTitle></CardHeader>
        <CardContent className='flex flex-wrap items-end gap-2'>
          <Input className='max-w-64' placeholder='规则名' value={rule} onChange={(e) => setRule(e.target.value)} />
          <Button variant='outline' disabled={act.isPending} onClick={() => act.mutate('rearm')}>重新启用</Button>
          <Button variant='outline' disabled={act.isPending} onClick={() => act.mutate('resolve')}>处置活跃记录</Button>
          <Button disabled={act.isPending} onClick={() => act.mutate('dispatch')}>立即投递</Button>
        </CardContent>
        {(notice || act.error) && (
          <CardContent>
            {notice && <p className='text-sm text-emerald-600 dark:text-emerald-400'>{notice}</p>}
            {act.error && <p className='text-destructive text-sm'>{(act.error as Error).message}</p>}
          </CardContent>
        )}
      </Card>

      <Card>
        <CardContent className='p-0'>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>规则</TableHead><TableHead>严重级别</TableHead><TableHead>状态</TableHead><TableHead>尝试</TableHead><TableHead>最近错误</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((r: any, i: number) => (
                <TableRow key={i}>
                  <TableCell>
                    {r.id ? (
                      <Link className='underline-offset-4 hover:underline' to='/alerts/$alertId' params={{ alertId: r.id }}>
                        {r.rule ?? r.rule_name}
                      </Link>
                    ) : (
                      r.rule ?? r.rule_name
                    )}
                  </TableCell>
                  <TableCell className='text-muted-foreground'>{r.severity ?? '—'}</TableCell>
                  <TableCell><Badge variant={String(r.status).includes('DEAD') ? 'destructive' : 'outline'}>{r.status}</Badge></TableCell>
                  <TableCell className='text-muted-foreground'>{r.attempts ?? 0}</TableCell>
                  <TableCell className='text-muted-foreground'>{r.last_error ?? '—'}</TableCell>
                </TableRow>
              ))}
              {rows.length === 0 && (
                <TableRow><TableCell colSpan={5} className='text-muted-foreground py-8 text-center text-sm'>没有匹配的发件箱记录</TableCell></TableRow>
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>
    </div>
  )
}
