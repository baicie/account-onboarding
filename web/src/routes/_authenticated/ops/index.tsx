import { createFileRoute } from '@tanstack/react-router'
import { OpsPage } from '@/features/ops'

export const Route = createFileRoute('/_authenticated/ops/')({
  component: OpsPage,
})
