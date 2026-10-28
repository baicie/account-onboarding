import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { api } from '@/lib/api'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'

const DECISIONS = [
  { value: 'RESOURCE_CONFIRMED', label: '已确认远端资源存在，关闭' },
  { value: 'NO_RESOURCE_CREATED', label: '已确认未创建任何资源，关闭' },
  { value: 'MANUAL_COMPENSATION_COMPLETED', label: '已完成人工补偿，关闭' },
]

export function OpsDetailPage({ opId }: { opId: string }) {
  const qc = useQueryClient()
  const [decision, setDecision] = useState(DECISIONS[0].value)
  const [reason, setReason] = useState('')
  const [evidence, setEvidence] = useState('')
  const audit = useQuery({ queryKey: ['external', opId, 'audit'], queryFn: () => api.externalAudit(opId) })
  const resolve = useMutation({
    mutationFn: () => api.resolveExternal(opId, { decision, reason, evidence_reference: evidence }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['external', opId] }),
  })
  const entries: any[] = audit.data?.items ?? []

  return (
    <div className='flex flex-col gap-4'>
      <div className='flex items-center gap-3'>
        <Button asChild variant='ghost' size='sm'><Link to='/ops'>← 返回对账</Link></Button>
        <h1 className='text-2xl font-bold tracking-tight'>外部操作</h1>
      </div>

      <Card>
        <CardHeader className='pb-3'><CardTitle className='text-sm font-medium'>记录决策</CardTitle></CardHeader>
        <CardContent className='flex flex-col gap-3'>
          <div className='flex flex-col gap-1.5'>
            <Label>决策</Label>
            <Select value={decision} onValueChange={setDecision}>
              <SelectTrigger><SelectValue /></SelectTrigger>
              <SelectContent>
                {DECISIONS.map((d) => <SelectItem key={d.value} value={d.value}>{d.label}</SelectItem>)}
              </SelectContent>
            </Select>
          </div>
          <div className='flex flex-col gap-1.5'>
            <Label htmlFor='reason'>原因（必填）</Label>
            <Input id='reason' value={reason} onChange={(e) => setReason(e.target.value)} />
          </div>
          <div className='flex flex-col gap-1.5'>
            <Label htmlFor='evidence'>证据引用（工单/控制台编号，绝不填写令牌）</Label>
            <Input id='evidence' value={evidence} onChange={(e) => setEvidence(e.target.value)} />
          </div>
          {resolve.error && <p className='text-destructive text-sm'>{(resolve.error as Error).message}</p>}
          <div>
            <Button disabled={resolve.isPending || reason.trim() === ''} onClick={() => resolve.mutate()}>记录决策</Button>
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader className='pb-3'><CardTitle className='text-sm font-medium'>审计历史</CardTitle></CardHeader>
        <CardContent className='p-0'>
          <Table>
            <TableHeader>
              <TableRow><TableHead>时间</TableHead><TableHead>动作</TableHead><TableHead>操作者</TableHead><TableHead>决策</TableHead></TableRow>
            </TableHeader>
            <TableBody>
              {entries.map((e: any, i: number) => (
                <TableRow key={i}>
                  <TableCell className='text-muted-foreground'>{e.at ?? e.created_at ?? '—'}</TableCell>
                  <TableCell>{e.action ?? e.kind ?? '—'}</TableCell>
                  <TableCell className='text-muted-foreground'>{e.actor ?? '—'}</TableCell>
                  <TableCell>{e.decision ? <Badge variant='secondary'>{e.decision}</Badge> : '—'}</TableCell>
                </TableRow>
              ))}
              {entries.length === 0 && (
                <TableRow><TableCell colSpan={4} className='text-muted-foreground py-8 text-center text-sm'>暂无审计历史</TableCell></TableRow>
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>
    </div>
  )
}
