export function BrandMark({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 90 90" className={className} aria-hidden="true">
      <rect width="90" height="90" rx="18" className="fill-primary" />
      <g fill="none" className="stroke-primary-foreground">
        <circle cx="45" cy="30" r="9" strokeWidth="4.5" />
        <path
          d="M45 39V67M28 47H62M23 53C23 72 67 72 67 53"
          strokeWidth="4.5"
          strokeLinecap="round"
        />
        <path
          d="M20 49L23 54L28 52M70 49L67 54L62 52"
          strokeWidth="3.5"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
      </g>
    </svg>
  );
}
