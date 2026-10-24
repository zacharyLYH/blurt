---
name: shadcn-ui
description: Add or change shadcn components in Blurt's web app. Use when a component is missing, a variant is not exported, `asChild` does not exist, or you are about to run the shadcn CLI.
---

# shadcn in Blurt

`web/components.json` is configured (style `new-york`, base color `neutral`, `cssVariables`, `iconLibrary: lucide`), so the CLI knows where files go. What it writes is not what this repo wants.

## The three rules this repo adds

1. **No Radix.** Everything in `web/src/components/ui/` is plain HTML: no `Slot`, no `data-slot`, no `forwardRef` (React 19 props typing), and no `@radix-ui/*` in `package.json`. Upstream components that need Radix — Separator, ScrollArea, Tooltip, Dialog, Sheet, Sidebar — are deliberately absent, so an overflow list uses native `overflow-y-auto` and a divider is a `border-t`.
2. **`asChild` is hand-rolled where it is needed.** `Button` has none. `Item` has one that clones the child and merges **only its classes**: a child renders its own props, so put `to`/`data-testid`/`onClick` on the child, never on `Item`.
3. **Only what is used ships.** `Card` has no `CardFooter`/`CardAction`; `Alert` has no `AlertTitle`; `Item` has no `ItemMedia`/`ItemSeparator`/`ItemHeader`/`ItemFooter`. `cva` variant maps are **not exported** (`buttonVariants`, `itemVariants`, `badgeVariants`) — that is also what keeps `react-refresh/only-export-components` quiet.

Installed today: `alert`, `badge`, `button`, `card`, `input`, `item`, `label`, `skeleton`, `textarea`.

## Adding one

Read the upstream source for the `new-york-v4` style, then adapt it:

```sh
curl -s https://ui.shadcn.com/r/styles/new-york-v4/item.json \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["files"][0]["content"])'
```

Swap `cn` to `@/lib/utils`, strip `data-slot`/`Slot`, drop `dark:` variants, delete the exports nothing uses. Do **not** run `npx shadcn add`: it writes upstream files and pulls `radix-ui` into the dependency list, which rule 1 exists to avoid.

## Keep the tests' eyes on

Refactors here are markup-only, but the e2e suite asserts real surfaces: `data-testid` (`open-jobs`, `open-job`, `permissions-mic`, `permissions-notifications`, `capture-text`, `job-error`), `role="alert"` with the exact message text (`auth.spec.ts`), and that a sidebar row is still an `<a href>`. `Alert` supplies the alert role itself; keep messages in `AlertDescription` alone so `toHaveText` still matches exactly.