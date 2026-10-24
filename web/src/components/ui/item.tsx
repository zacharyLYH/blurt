import { cloneElement, isValidElement } from 'react'
import { cva, type VariantProps } from 'class-variance-authority'
import { cn } from '@/lib/utils'

const itemVariants = cva(
  'group/item flex flex-wrap items-center rounded-md border border-transparent text-sm transition-colors outline-none [a]:hover:bg-accent/50 focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50',
  {
    variants: {
      variant: {
        default: 'bg-transparent',
        outline: 'border-border',
        muted: 'bg-muted/50',
      },
      size: {
        default: 'gap-4 p-4',
        sm: 'gap-2.5 px-4 py-3',
      },
    },
    defaultVariants: { variant: 'default', size: 'default' },
  },
)

// ItemGroup is the list a row belongs to. It carries the list role itself, so a
// sidebar does not need its own nav element around it.
function ItemGroup({ className, ...props }: React.ComponentProps<'div'>) {
  return <div role="list" className={cn('group/item-group flex flex-col', className)} {...props} />
}

// Item is one row. asChild renders the caller's own element instead of a wrapper
// div, which is what lets a row be a Link; upstream shadcn does this with Radix
// Slot, and Blurt keeps its dependency list minimal instead. A child renders its
// own props: only the row's classes are merged in.
function Item({
  className,
  variant,
  size,
  asChild = false,
  children,
  ...props
}: React.ComponentProps<'div'> & VariantProps<typeof itemVariants> & { asChild?: boolean }) {
  const classes = cn(itemVariants({ variant, size }), className)
  if (asChild && isValidElement(children)) {
    const child = children as React.ReactElement<{ className?: string }>
    return cloneElement(child, { className: cn(classes, child.props.className) })
  }
  return (
    <div className={classes} {...props}>
      {children}
    </div>
  )
}

// min-w-0 is what lets a long title truncate inside the row rather than push it
// wider than its column.
function ItemContent({ className, ...props }: React.ComponentProps<'div'>) {
  return <div className={cn('flex min-w-0 flex-1 flex-col gap-1', className)} {...props} />
}

function ItemTitle({ className, ...props }: React.ComponentProps<'div'>) {
  return <div className={cn('flex w-fit items-center gap-2 text-sm leading-snug font-medium', className)} {...props} />
}

function ItemDescription({ className, ...props }: React.ComponentProps<'p'>) {
  return (
    <p
      className={cn('line-clamp-2 text-sm leading-normal text-balance text-muted-foreground', className)}
      {...props}
    />
  )
}

function ItemActions({ className, ...props }: React.ComponentProps<'div'>) {
  return <div className={cn('flex items-center gap-2', className)} {...props} />
}

export { Item, ItemGroup, ItemContent, ItemTitle, ItemDescription, ItemActions }