---
name: shadcn-ui
description: Add or change shadcn components in Blurt's web app. Use when a component is missing, a variant is not exported, `asChild` does not exist, or you are about to run the shadcn CLI.
---

# shadcn in Blurt

`web/components.json` is configured (style `new-york`, base color `neutral`, `cssVariables`, `iconLibrary: lucide`), so the CLI knows where files go. What it writes is not what this repo wants.

## The three rules this repo adds

1. **Radix only under a vendored component.** `collapsible`, `radio-group` and `dropdown-menu` are the three that need it; nothing else pulls `@radix-ui/*`. Everything else in `web/src/components/ui/` is plain HTML: no `Slot`, no `data-slot`, no `forwardRef` (React 19 props typing). Components that would need a new primitive — Dialog, Sheet, Tooltip, ScrollArea, Sidebar — stay absent: an overflow list is native `overflow-y-auto`, a divider is a `border-t`.
2. **`asChild` is hand-rolled where it is needed.** `Button` has none. `Item` has one that clones the child and merges **only its classes**: a child renders its own props, so put `to`/`data-testid`/`onClick` on the child, never on `Item`.
3. **Only what is used ships.** `Card` has no `CardFooter`/`CardAction`; `Alert` has no `AlertTitle`; `Item` has no `ItemMedia`/`ItemSeparator`/`ItemHeader`/`ItemFooter`. `cva` variant maps are **not exported** (`buttonVariants`, `itemVariants`, `badgeVariants`) — that is also what keeps `react-refresh/only-export-components` quiet.

Installed today: `alert`, `badge`, `button`, `calendar`, `card`, `collapsible`, `dropdown-menu`, `input`, `item`, `label`, `radio-group`, `skeleton`, `textarea`.

Use them before writing markup. A status chip, a count or a tag is a `Badge`; a panel is a `Card`; a row is an `Item`; a toggle is a `Button`; a disclosure is a `Collapsible`. Hand-rolled `<button>`/`<span>` with pill or chip classes is the smell this file exists to stop.

## Adding one

Read the upstream source for the `new-york-v4` style, then adapt it:

```sh
curl -s https://ui.shadcn.com/r/styles/new-york-v4/item.json \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["files"][0]["content"])'
```

Swap `cn` to `@/lib/utils`, strip `data-slot`/`Slot`, drop `dark:` variants, delete the exports nothing uses. Do **not** run `npx shadcn add`: it writes upstream files and pulls `radix-ui` into the dependency list, which rule 1 exists to avoid.

## Menus vs disclosures

A panel that must close on an outside click, on Escape, and return focus is a `DropdownMenu`: Radix owns that layer, so the page never grows a scrim `<button>` or a `keydown` handler of its own. Rows are `DropdownMenuItem asChild` wrapping the caller's own element (a `Link`), styled with the same border/padding as an `Item`.

A plain disclosure that stays put — the feedback form — is a `Collapsible` with the trigger as a `Button`: `aria-expanded`/`aria-controls` come with it, and open state is the only state to hold.

## Keep the tests' eyes on

Refactors here are markup-only, but the e2e suite asserts real surfaces: `data-testid` (`inbox`, `inbox-item`, `inbox-toggle`, `permissions-mic`, `permissions-notifications`, `capture-text`, `reply-text`, `job-error`, `exit-chat`, `day-modal`), `role="alert"` with the exact message text (`auth.spec.ts`), and that an inbox row is still an `<a href>`. `Alert` supplies the alert role itself; keep messages in `AlertDescription` alone so `toHaveText` still matches exactly.