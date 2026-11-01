import * as CollapsiblePrimitive from '@radix-ui/react-collapsible'

// shadcn Collapsible: the feedback form is offered, not pushed. The trigger and the
// content stay one primitive so open state is the only state to manage.
const Collapsible = CollapsiblePrimitive.Root
const CollapsibleTrigger = CollapsiblePrimitive.Trigger
const CollapsibleContent = CollapsiblePrimitive.Content

export { Collapsible, CollapsibleTrigger, CollapsibleContent }
