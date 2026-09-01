import React from 'react'
import ReactDOM from 'react-dom/client'
import App from './App'

class RuntimeErrorBoundary extends React.Component<React.PropsWithChildren, { error: string | null }> {
  state = { error: null as string | null }

  static getDerivedStateFromError(error: unknown) {
    return { error: error instanceof Error ? error.message : 'Unknown interface error' }
  }

  componentDidCatch(error: Error, info: React.ErrorInfo) {
    console.error('WhiskerLink interface failure', error, info)
  }

  render() {
    if (this.state.error) {
      return <main className="fatal-screen"><p>INTERFACE FAILURE</p><h1>WhiskerLink could not finish loading.</h1><code>{this.state.error}</code></main>
    }
    return this.props.children
  }
}

ReactDOM.createRoot(document.getElementById('root') as HTMLElement).render(
  <React.StrictMode>
    <RuntimeErrorBoundary><App /></RuntimeErrorBoundary>
  </React.StrictMode>,
)
