import { useQuery } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'

// Operator view of what the server can actually reach. A provider marked fake never touches a
// network; a live one is talking to a real service, which is what turns this from a demo into a
// working chain.
export function ProvidersPage() {
  const { data, isLoading, error } = useQuery({ queryKey: ['providers'], queryFn: () => api.providers() })
  const items: any[] = data?.items ?? []
  const live = items.filter((p) => p.live).length

  return (
    <div className='flex flex-col gap-4'>
      <div>
        <h1 className='text-2xl font-bold tracking-tight'>服务方</h1>
        <p className='text-muted-foreground text-sm'>
          运行模式 {data?.mode ?? '—'}；共 {items.length} 个服务方，其中 {live} 个接通了真实服务。
        </p>
      </div>
      <Card>
        <CardHeader className='pb-3'>
          <CardTitle className='text-sm font-medium'>适配器状态</CardTitle>
        </CardHeader>
        <CardContent className='p-0'>
          {isLoading && <p className='text-muted-foreground p-4 text-sm'>加载中…</p>}
          {error && <p className='text-destructive p-4 text-sm'>{(error as Error).message}</p>}
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>名称</TableHead>
                <TableHead>类型</TableHead>
                <TableHead>启用</TableHead>
                <TableHead>真实</TableHead>
                <TableHead>限制原因</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {items.map((p) => (
                <TableRow key={`${p.kind}/${p.name}`}>
                  <TableCell className='font-medium'>{p.name}</TableCell>
                  <TableCell className='text-muted-foreground'>{p.kind}</TableCell>
                  <TableCell>
                    <Badge variant={p.enabled ? 'secondary' : 'outline'}>{p.enabled ? '已启用' : '未启用'}</Badge>
                  </TableCell>
                  <TableCell>
                    <Badge variant={p.live ? 'default' : 'outline'}>{p.live ? '真实' : '模拟'}</Badge>
                  </TableCell>
                  <TableCell className='text-muted-foreground'>{p.restriction_reason ?? '—'}</TableCell>
                </TableRow>
              ))}
              {items.length === 0 && !isLoading && (
                <TableRow>
                  <TableCell colSpan={5} className='text-muted-foreground py-8 text-center text-sm'>
                    没有已注册的服务方
                  </TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>
      <p className='text-muted-foreground text-xs'>
        接入真实服务方：Cloudflare 临时邮箱需 ONBOARD_CLOUDFLARE_ENABLED 与 BASE_URL/域名/管理员密码；
        GitHub 用 PAT 做凭据校验；WorkBuddy 需要 BASE_URL 与令牌。自建实例请把主机名加入
        ONBOARD_CLOUDFLARE_ALLOWED_HOSTS。
      </p>
    </div>
  )
}
