import { createFileRoute } from '@tanstack/react-router'
import { RunsPage } from '@/features/runs'

export const Route = createFileRoute('/_authenticated/')({
  component: RunsPage,
})
