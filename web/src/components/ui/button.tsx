import type { VariantProps } from 'class-variance-authority'
import { buttonVariants } from '@/components/ui/button-variants'
import { cn } from '@/lib/utils'

function Button({
  className,
  variant,
  size,
  ...props
}: React.ComponentProps<'button'> & VariantProps<typeof buttonVariants>) {
  return <button className={cn(buttonVariants({ variant, size, className }))} {...props} />
}

export { Button }
