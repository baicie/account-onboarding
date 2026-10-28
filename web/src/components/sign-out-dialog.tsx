import { ConfirmDialog } from '@/components/confirm-dialog'
import { logout } from '@/lib/session'

interface SignOutDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
}

export function SignOutDialog({ open, onOpenChange }: SignOutDialogProps) {
  return (
    <ConfirmDialog
      open={open}
      onOpenChange={onOpenChange}
      title='退出登录'
      desc='会话将被服务器吊销，返回登录页。'
      confirmText='退出登录'
      handleConfirm={() => void logout()}
    />
  )
}
