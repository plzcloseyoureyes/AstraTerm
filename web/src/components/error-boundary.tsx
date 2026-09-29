import * as React from 'react'
import { Bug, RotateCcw } from 'lucide-react'
import { Button } from './ui/button'

interface Props {
  children: React.ReactNode
  /** Label of the failing area, shown in the fallback. */
  label?: string
  /** Custom fallback renderer. */
  fallback?: (error: Error, reset: () => void) => React.ReactNode
  /** Changing this value resets the boundary. */
  resetKey?: unknown
}

interface State {
  error: Error | null
}

/** Contains render errors of one area (a tab, a panel) so the rest of the app keeps working. */
export class ErrorBoundary extends React.Component<Props, State> {
  state: State = { error: null }

  static getDerivedStateFromError(error: Error): State {
    return { error }
  }

  componentDidCatch(error: Error, info: React.ErrorInfo) {
    console.error(`[ErrorBoundary${this.props.label ? `: ${this.props.label}` : ''}]`, error, info.componentStack)
  }

  componentDidUpdate(prev: Props) {
    if (this.state.error && prev.resetKey !== this.props.resetKey) this.setState({ error: null })
  }

  reset = () => this.setState({ error: null })

  render() {
    const { error } = this.state
    if (!error) return this.props.children
    if (this.props.fallback) return this.props.fallback(error, this.reset)
    return (
      <div role="alert" className="flex h-full min-h-32 w-full flex-col items-center justify-center gap-3 p-6 text-center">
        <div className="flex size-10 items-center justify-center rounded-xl border bg-destructive/10 text-destructive">
          <Bug className="size-5" />
        </div>
        <div className="grid gap-1">
          <div className="font-medium">{this.props.label ? `${this.props.label} crashed` : 'Something went wrong'}</div>
          <div className="max-w-md font-mono text-sm break-words text-muted-foreground">{error.message}</div>
        </div>
        <Button size="sm" variant="secondary" onClick={this.reset}>
          <RotateCcw /> Try again
        </Button>
      </div>
    )
  }
}
