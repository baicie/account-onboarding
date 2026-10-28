import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { api } from '../lib/api'
import { Button, Card, CardBody, CardHeader, Input } from '../components/ui'

export default function CreateRun() {
  const navigate = useNavigate()
  const [form, setForm] = useState({
    template: 'sandbox-email-verify-oidc',
    mailbox_provider: 'mailpit',
    identity_provider: 'keycloak',
    oauth_provider: 'keycloak',
    email_provider: '',
    existing_identity_ref: '',
  })
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  function set(k: keyof typeof form) {
    return (e: React.ChangeEvent<HTMLInputElement>) => setForm({ ...form, [k]: e.target.value })
  }

  async function submit(e: React.FormEvent) {
    e.preventDefault()
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
      navigate(id ? `/runs/${id}` : '/')
    } catch (e: any) {
      setError(String(e.message ?? e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className='max-w-2xl space-y-4'>
      <h1 className='text-xl font-semibold'>创建运行</h1>
      <Card>
        <CardHeader><span className='text-sm font-medium'>运行参数</span></CardHeader>
        <CardBody>
          <form className='space-y-3' onSubmit={submit}>
            {(['template', 'mailbox_provider', 'identity_provider', 'oauth_provider', 'email_provider', 'existing_identity_ref'] as const).map((k) => (
              <label key={k} className='block text-sm'>
                <span className='mb-1 block text-muted-foreground'>{k}</span>
                <Input value={form[k]} onChange={set(k)} />
              </label>
            ))}
            {error && <p className='text-sm text-destructive'>{error}</p>}
            <div className='flex gap-2 pt-2'>
              <Button type='submit' disabled={busy}>创建运行</Button>
              <Button type='button' variant='outline' onClick={() => navigate('/')}>返回</Button>
            </div>
          </form>
        </CardBody>
      </Card>
      <p className='text-xs text-muted-foreground'>
        真实账号模板只作用于你已持有的账号；本服务不会创建第三方账号，也不会自动化授权同意。
      </p>
    </div>
  )
}
