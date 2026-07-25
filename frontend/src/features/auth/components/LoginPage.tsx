import { useLocation } from 'react-router'
import { Card } from '../../../shared/ui/Card'

export function LoginPage() {
  const location = useLocation()
  const message = (location.state as { message?: string } | null)?.message

  return (
    <Card>
      <h1>Login</h1>
      {message && <p>{message}</p>}
    </Card>
  )
}
