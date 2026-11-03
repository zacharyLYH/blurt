// The hero's background: a current of curves with a lit dash travelling along
// each one. Pure CSS on top of one SVG, so the motion costs no JavaScript frames
// and a reduced-motion browser gets a still field instead.
const curves = Array.from({ length: 22 }, (_, index) => {
  // One wave, stepped down the screen: the lines read as a single current.
  const y = index * 52 - 220
  return `M -200 ${y} C 120 ${y + 110}, 420 ${y - 140}, 720 ${y + 40} S 1160 ${y - 80}, 1480 ${y + 120}`
})

export function BackgroundPaths({ scrim = true }: { scrim?: boolean }) {
  return (
    <div className="pointer-events-none absolute inset-0 -z-10 overflow-hidden" aria-hidden="true">
      <svg
        viewBox="0 0 1280 800"
        preserveAspectRatio="xMidYMid slice"
        className="size-full opacity-60 [mask-image:radial-gradient(70%_55%_at_50%_20%,transparent_0%,black_72%)] lg:opacity-70"
      >
        <defs>
          <linearGradient id="blurt-path-fade" x1="0" y1="0" x2="1" y2="0">
            <stop offset="0%" stopColor="var(--neon)" stopOpacity="0" />
            <stop offset="35%" stopColor="var(--neon)" stopOpacity="0.85" />
            <stop offset="70%" stopColor="var(--neon)" stopOpacity="0.25" />
            <stop offset="100%" stopColor="var(--neon)" stopOpacity="0" />
          </linearGradient>
        </defs>

        {/* The field itself: barely-there lines that give the fan its shape. */}
        {curves.map((d, index) => (
          <path
            key={`line-${index}`}
            d={d}
            fill="none"
            stroke="currentColor"
            strokeWidth={1}
            className={index % 4 === 0 ? 'text-neon/15' : 'text-foreground/10'}
          />
        ))}

        {/* The light that travels them, each at its own pace. The dash pattern
            repeats every 540 units, so the loop has no seam. */}
        {curves.map((d, index) => (
          <path
            key={`dash-${index}`}
            d={d}
            fill="none"
            stroke="url(#blurt-path-fade)"
            strokeWidth={index % 5 === 0 ? 2 : 1.4}
            strokeLinecap="round"
            strokeDasharray="120 420"
            className="blurt-path"
            style={{
              animation: `blurt-path ${18 + (index % 7) * 3}s linear ${-index * 1.4}s infinite`,
              // The still frame has to be composed too. Without an offset of its
              // own every dash would sit at the same point the moment the motion
              // stops (reduced motion, a screenshot with animations off), and the
              // field would read as one bright band cut off at both sides.
              strokeDashoffset: -index * 27,
            }}
          />
        ))}
      </svg>

      {/* Two soft lights behind the type, so the neon has something to sit on. */}
      <div className="absolute -top-40 left-1/2 size-[38rem] -translate-x-1/2 rounded-full bg-neon/10 blur-[120px]" />
      <div className="absolute -bottom-52 right-0 size-[32rem] rounded-full bg-primary/10 blur-[120px]" />

      {/* On a wide screen the copy sits on a scrim, so the field stays a
          backdrop for the phone rather than a texture behind the words. */}
      {scrim && (
        <div className="absolute inset-y-0 left-0 hidden w-[58%] bg-gradient-to-r from-background via-background via-80% to-transparent lg:block" />
      )}
    </div>
  )
}