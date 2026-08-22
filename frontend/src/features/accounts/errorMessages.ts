import { isApiError } from '../../shared/api-client/ApiError'

// Never fall back to showing a 0 balance here — an honest "unavailable"
// beats a fake number in a banking product.
export function getAccountErrorMessage(error: unknown): string {
  if (isApiError(error) && error.status === 503) {
    return 'Баланс временно недоступен. Попробуйте ещё раз через минуту.'
  }
  if (isApiError(error) && error.status === 404) {
    return 'Ваш счёт ещё создаётся — это обычно занимает несколько секунд. Попробуйте обновить.'
  }
  return 'Не удалось загрузить данные счёта, попробуйте ещё раз.'
}
