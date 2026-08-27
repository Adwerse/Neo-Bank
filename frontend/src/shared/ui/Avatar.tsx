import { avatarColor } from './avatarColor'
import styles from './Avatar.module.css'

interface AvatarProps {
  // Presigned thumbnail URL, when one exists.
  imageUrl?: string | null
  // Stable per-account seed for the fallback color — pass user_id, never
  // email or display name (see avatarColor.ts).
  seed: string
  initial: string
  size?: number
  className?: string
}

// Real avatar image when one exists, otherwise initials on a color
// deterministically derived from `seed` — same color every session, never
// random. Used identically in MobileShell/Sidebar's header chrome and on
// the profile screen itself, just at different sizes.
export function Avatar({ imageUrl, seed, initial, size = 38, className }: AvatarProps) {
  const style = { width: size, height: size, fontSize: Math.round(size * 0.4) }
  const classes = [styles.avatar, className].filter(Boolean).join(' ')

  if (imageUrl) {
    return <img src={imageUrl} alt="" className={classes} style={style} />
  }

  return (
    <div className={classes} style={{ ...style, background: avatarColor(seed) }} aria-hidden="true">
      {initial}
    </div>
  )
}
