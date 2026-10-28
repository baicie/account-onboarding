import type { ButtonHTMLAttributes, HTMLAttributes, InputHTMLAttributes } from 'react'

export function cx(...parts: Array<string | false | null | undefined>) {
  return parts.filter(Boolean).join(' ')
}

type ButtonProps = ButtonHTMLAttributes<HTMLButtonElement> & { variant?: 'default' | 'outline' | 'destructive' | 'ghost'; size?: 'sm' | 'md' }

export function Button({ variant = 'default', size = 'md', className, ...props }: ButtonProps) {
  const base = 'inline-flex items-center justify-center rounded-md font-medium transition-colors disabled:opacity-50 disabled:pointer-events-none'
  const variants = {
    default: 'bg-primary text-primary-foreground hover:opacity-90',
    outline: 'border border-border bg-transparent hover:bg-muted',
    destructive: 'bg-destructive text-white hover:opacity-90',
    ghost: 'hover:bg-muted',
  } as const
  const sizes = { sm: 'h-8 px-3 text-sm', md: 'h-9 px-4 text-sm' } as const
  return <button className={cx(base, variants[variant], sizes[size], className)} {...props} />
}

export function Input({ className, ...props }: InputHTMLAttributes<HTMLInputElement>) {
  return <input className={cx('h-9 w-full rounded-md border border-border bg-background px-3 text-sm outline-none focus:ring-2 focus:ring-primary/40', className)} {...props} />
}

export function Card({ className, ...props }: HTMLAttributes<HTMLDivElement>) {
  return <div className={cx('rounded-lg border border-border bg-card text-card-foreground', className)} {...props} />
}

export function CardHeader({ className, ...props }: HTMLAttributes<HTMLDivElement>) {
  return <div className={cx('flex items-center justify-between gap-3 border-b border-border px-4 py-3', className)} {...props} />
}

export function CardBody({ className, ...props }: HTMLAttributes<HTMLDivElement>) {
  return <div className={cx('p-4', className)} {...props} />
}

export function Badge({ tone = 'muted', children }: { tone?: 'muted' | 'ok' | 'warn' | 'bad'; children: React.ReactNode }) {
  const tones = {
    muted: 'bg-muted text-muted-foreground',
    ok: 'bg-emerald-500/15 text-emerald-400',
    warn: 'bg-amber-500/15 text-amber-400',
    bad: 'bg-destructive/15 text-destructive',
  } as const
  return <span className={cx('inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium', tones[tone])}>{children}</span>
}

export function Table({ head, children }: { head: string[]; children: React.ReactNode }) {
  return (
    <div className='overflow-x-auto'>
      <table className='w-full text-sm'>
        <thead>
          <tr className='border-b border-border text-left text-muted-foreground'>
            {head.map((h) => <th key={h} className='px-3 py-2 font-medium whitespace-nowrap'>{h}</th>)}
          </tr>
        </thead>
        <tbody>{children}</tbody>
      </table>
    </div>
  )
}
