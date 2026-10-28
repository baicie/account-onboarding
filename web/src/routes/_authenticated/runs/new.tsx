import { createFileRoute } from '@tanstack/react-router'
import { CreateRunPage } from '@/features/runs/create'

export const Route = createFileRoute('/_authenticated/runs/new')({
  component: CreateRunPage,
})
