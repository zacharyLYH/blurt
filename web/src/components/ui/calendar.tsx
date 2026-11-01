import { DayPicker, type DayPickerProps } from 'react-day-picker'
import 'react-day-picker/style.css'
import { cn } from '@/lib/utils'

// shadcn's calendar is react-day-picker under a thin wrapper. The month grid,
// leap years, week boundaries and keyboard movement are the library's; Blurt only
// supplies the theme classes and, on the page, the day contents.
export function Calendar({ className, ...props }: DayPickerProps) {
  return (
    <DayPicker
      weekStartsOn={0}
      showOutsideDays={false}
      className={cn('rdp-blurt w-full', className)}
      {...props}
    />
  )
}
