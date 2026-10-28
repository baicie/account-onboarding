import { useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { api } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'

const FIELDS = [
  ['template', 'sandbox-email-verify-oidc'],
  ['mailbox_provider', 'mailpit'],
  ['identity_provider', 'keycloak'],
  ['oauth_provider', 'keycloak'],
  ['email_provider', ''],
  ['existing_identity_ref', ''],
] as const

export function CreateRunPage() {
  const navigate = useNavigate()
  const [form, setForm] = useState<Record<string, string>>(
    Object.fromEntries(FIELDS) as Record<string, string>
  )
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  async function submit(event: React.FormEvent) {
    event.preventDefault()
    setBusy(true)
    setError('')
    try {
      const body: Record<string, string> = {
        template: form.template,
        mailbox_provider: form.mailbox_provider,
        identity_provider: form.identity_provider,
        oauth_provider: form.oauth_provider,
      }
      if (form.email_provider) body.email_provider = form.email_provider
      if (form.existing_identity_ref) body.existing_identity_ref = form.existing_identity_ref
      const created = await api.createRun(body)
      const id = created?.run?.id ?? created?.id
      if (id) navigate({ to: '/runs/$runId', params: { runId: id } })
      else navigate({ to: '/' })
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className='max-w-2xl'>
      <h1 className='mb-4 text-2xl font-bold tracking-tight'>创建运行</h1>
      <Card>
        <CardHeader>
          <CardTitle className='text-sm font-medium'>运行参数</CardTitle>
        </CardHeader>
        <CardContent>
          <form className='flex flex-col gap-4' onSubmit={submit}>
            {FIELDS.map(([key]) => (
              <div key={key} className='flex flex-col gap-1.5'>
                <Label htmlFor={key}>{key}</Label>
                <Input
                  id={key}
                  value={form[key]}
                  onChange={(e) => setForm({ ...form, [key]: e.target.value })}
                />
              </div>
            ))}
            {error && <p className='text-destructive text-sm'>{error}</p>}
            <div className='flex gap-2'>
              <Button type='submit' disabled={busy}>创建运行</Button>
              <Button type='button' variant='outline' onClick={() => navigate({ to: '/' })}>返回</Button>
            </div>
          </form>
        </CardContent>
      </Card>
      <p className='text-muted-foreground mt-3 text-xs'>
        真实账号模板只作用于你已持有的账号；本服务不会创建第三方账号，也不会自动化授权同意。
      </p>
    </div>
  )
}
