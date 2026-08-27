import { request } from '../../shared/api-client/client'
import type { paths } from '../../shared/api-client/schema'

type Profile = paths['/profile']['get']['responses']['200']['content']['application/json']
type UpdateProfileRequest = paths['/profile']['patch']['requestBody']['content']['application/json']
type UploadAvatarURLResponse =
  paths['/profile/avatar/upload-url']['post']['responses']['200']['content']['application/json']

export function getProfile(): Promise<Profile> {
  return request<Profile>('/profile')
}

export function updateProfile(body: UpdateProfileRequest): Promise<Profile> {
  return request<Profile>('/profile', {
    method: 'PATCH',
    body: JSON.stringify(body),
  })
}

export function createAvatarUploadURL(): Promise<UploadAvatarURLResponse> {
  return request<UploadAvatarURLResponse>('/profile/avatar/upload-url', { method: 'POST' })
}

export function confirmAvatarUpload(key: string): Promise<Profile> {
  return request<Profile>('/profile/avatar/confirm', {
    method: 'POST',
    body: JSON.stringify({ key }),
  })
}

// Goes straight to object storage with the presigned POST policy from
// createAvatarUploadURL — never through request()/the Gateway, and never
// authenticated with this app's own bearer token (the policy itself is
// the credential). Uses XMLHttpRequest rather than fetch specifically
// because fetch has no cross-browser-reliable upload progress event;
// XHR's upload.onprogress does.
export function uploadAvatarFile(
  target: UploadAvatarURLResponse,
  file: Blob,
  onProgress: (fraction: number) => void,
): Promise<void> {
  return new Promise((resolve, reject) => {
    const form = new FormData()
    // Every policy field must be present as its own form field, and per
    // the S3/MinIO presigned-POST convention, the file field must come
    // LAST in the multipart body — FormData preserves append order, so
    // this loop-then-file sequence matters, not just which fields exist.
    for (const [field, value] of Object.entries(target.fields)) {
      form.append(field, value)
    }
    form.append('file', file)

    const xhr = new XMLHttpRequest()
    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable) onProgress(e.loaded / e.total)
    }
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) {
        resolve()
      } else {
        reject(new Error(`storage upload failed with status ${xhr.status}`))
      }
    }
    xhr.onerror = () => reject(new Error('network error during upload'))
    xhr.open('POST', target.url)
    xhr.send(form)
  })
}

export type { Profile, UpdateProfileRequest, UploadAvatarURLResponse }
