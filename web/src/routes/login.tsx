import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'

// The login form posts to the Go handler, which verifies the operator token, sets the session
// cookie and redirects back to the console. No JavaScript is required for this step.
function LoginPage() {
  const [token, setToken] = useState('')
  const error = new URLSearchParams(window.location.search).get('error') ?? ''
  return (
    <div className='flex min-h-svh items-center justify-center bg-muted/40 p-4'>
      <Card className='w-full max-w-sm'>
        <CardHeader>
          <CardTitle>运维登录</CardTitle>
          <CardDescription>该控制台只监听本机回环接口，并要求管理员令牌。</CardDescription>
        </CardHeader>
        <CardContent>
          <form className='flex flex-col gap-4' method='post' action='/ui/login'>
            <div className='flex flex-col gap-1.5'>
              <Label htmlFor='token'>管理员令牌</Label>
              <Input
                id='token'
                name='token'
                type='password'
                autoComplete='current-password'
                value={token}
                onChange={(e) => setToken(e.target.value)}
                required
              />
            </div>
            {error && <p className='text-destructive text-sm'>{error}</p>}
            <Button type='submit'>登录</Button>
          </form>
        </CardContent>
      </Card>
    </div>
  )
}

export const Route = createFileRoute('/login')({
  component: LoginPage,
})
