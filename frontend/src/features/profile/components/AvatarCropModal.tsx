import { useEffect, useRef, useState } from 'react'
import type { PointerEvent as ReactPointerEvent } from 'react'
import { Modal } from '../../../shared/ui/Modal'
import { Button } from '../../../shared/ui/Button'
import { ErrorText } from '../../../shared/ui/ErrorText'
import styles from './AvatarCropModal.module.css'

// Fixed square capture viewport (CSS px) and the resolution the actual
// crop is rendered at for upload — 512 gives real headroom over the
// backend's 256px thumbnail without being wasteful to upload.
const VIEWPORT = 260
const OUTPUT_SIZE = 512
const MAX_ZOOM = 3

interface Offset {
  x: number
  y: number
}

interface AvatarCropModalProps {
  file: File
  onCancel: () => void
  onCropped: (blob: Blob) => void
}

// No crop library in this project (consistent with shared/ui's existing
// preference for hand-rolled primitives over a dependency, see Modal.tsx's
// own comment) — this is a plain pan/zoom-over-a-fixed-square-viewport
// cropper: drag to pan, a range input to zoom, then a single canvas draw
// at confirm time. The circular overlay is only a preview aid (every
// avatar renders round via shared/ui/Avatar's border-radius) — the actual
// captured region, and what the backend receives, is the full square
// viewport regardless.
export function AvatarCropModal({ file, onCancel, onCropped }: AvatarCropModalProps) {
  const [image, setImage] = useState<HTMLImageElement | null>(null)
  const [loadError, setLoadError] = useState(false)
  const [zoom, setZoom] = useState(1)
  const [offset, setOffset] = useState<Offset>({ x: 0, y: 0 })
  const dragRef = useRef<{ pointerId: number; startX: number; startY: number; startOffset: Offset } | null>(null)

  useEffect(() => {
    const url = URL.createObjectURL(file)
    const img = new Image()
    img.onload = () => {
      const baseScale = VIEWPORT / Math.min(img.naturalWidth, img.naturalHeight)
      const w = img.naturalWidth * baseScale
      const h = img.naturalHeight * baseScale
      setImage(img)
      setZoom(1)
      setOffset({ x: (VIEWPORT - w) / 2, y: (VIEWPORT - h) / 2 })
    }
    // A file that passed the local type/size pre-check can still fail to
    // actually decode (truncated, corrupted, or a renamed non-image) —
    // without this the modal would just sit there with a disabled slider
    // and no explanation.
    img.onerror = () => setLoadError(true)
    img.src = url
    return () => URL.revokeObjectURL(url)
  }, [file])

  const baseScale = image ? VIEWPORT / Math.min(image.naturalWidth, image.naturalHeight) : 1
  const scale = baseScale * zoom
  const displayWidth = image ? image.naturalWidth * scale : 0
  const displayHeight = image ? image.naturalHeight * scale : 0

  function clamp(candidate: Offset, w: number, h: number): Offset {
    const minX = Math.min(0, VIEWPORT - w)
    const minY = Math.min(0, VIEWPORT - h)
    return {
      x: Math.min(0, Math.max(minX, candidate.x)),
      y: Math.min(0, Math.max(minY, candidate.y)),
    }
  }

  function handlePointerDown(e: ReactPointerEvent<HTMLDivElement>) {
    e.currentTarget.setPointerCapture(e.pointerId)
    dragRef.current = { pointerId: e.pointerId, startX: e.clientX, startY: e.clientY, startOffset: offset }
  }

  function handlePointerMove(e: ReactPointerEvent<HTMLDivElement>) {
    const drag = dragRef.current
    if (!drag || drag.pointerId !== e.pointerId) return
    const next = { x: drag.startOffset.x + (e.clientX - drag.startX), y: drag.startOffset.y + (e.clientY - drag.startY) }
    setOffset(clamp(next, displayWidth, displayHeight))
  }

  function endDrag() {
    dragRef.current = null
  }

  function handleZoomChange(nextZoom: number) {
    setZoom(nextZoom)
    if (!image) return
    const nextScale = baseScale * nextZoom
    const w = image.naturalWidth * nextScale
    const h = image.naturalHeight * nextScale
    setOffset((current) => clamp(current, w, h))
  }

  function handleConfirm() {
    if (!image) return
    const canvas = document.createElement('canvas')
    canvas.width = OUTPUT_SIZE
    canvas.height = OUTPUT_SIZE
    const ctx = canvas.getContext('2d')
    if (!ctx) return
    // The visible viewport square, translated back into the source
    // image's own pixel coordinates.
    const sourceX = -offset.x / scale
    const sourceY = -offset.y / scale
    const sourceSize = VIEWPORT / scale
    ctx.drawImage(image, sourceX, sourceY, sourceSize, sourceSize, 0, 0, OUTPUT_SIZE, OUTPUT_SIZE)
    canvas.toBlob(
      (blob) => {
        if (blob) onCropped(blob)
      },
      'image/jpeg',
      0.92,
    )
  }

  return (
    <Modal isOpen onClose={onCancel} title="Crop your photo">
      <div
        className={styles.viewport}
        style={{ width: VIEWPORT, height: VIEWPORT }}
        onPointerDown={handlePointerDown}
        onPointerMove={handlePointerMove}
        onPointerUp={endDrag}
        onPointerCancel={endDrag}
      >
        {image && (
          <img
            src={image.src}
            alt=""
            draggable={false}
            className={styles.image}
            style={{ width: displayWidth, height: displayHeight, transform: `translate(${offset.x}px, ${offset.y}px)` }}
          />
        )}
        <div className={styles.mask} aria-hidden="true" />
      </div>

      {loadError && <ErrorText>Could not open that image — please try a different file.</ErrorText>}

      <label className={styles.zoomRow}>
        <span className={styles.zoomLabel}>Zoom</span>
        <input
          type="range"
          min={1}
          max={MAX_ZOOM}
          step={0.01}
          value={zoom}
          onChange={(e) => handleZoomChange(Number(e.target.value))}
          className={styles.zoomSlider}
          disabled={!image}
        />
      </label>

      <div className={styles.actions}>
        <Button type="button" onClick={handleConfirm} disabled={!image}>
          Crop and upload
        </Button>
        <Button type="button" variant="secondary" onClick={onCancel}>
          Cancel
        </Button>
      </div>
    </Modal>
  )
}
