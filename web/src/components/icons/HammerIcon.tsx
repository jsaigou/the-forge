export function HammerIcon({ className, size = 16 }: { className?: string; size?: number }) {
  return (
    <svg
      className={className}
      width={size}
      height={size}
      viewBox="0 0 20 20"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <rect x="8" y="1" width="11" height="5" rx="0.5" fill="currentColor" stroke="none" />
      <path d="M10 6 L4 17" strokeWidth="2.5" />
    </svg>
  );
}
