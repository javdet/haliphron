import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter } from 'react-router-dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { ApiError } from './api/client'
import { TokenGate } from './auth/TokenGate'
import App from './App'
import './styles.css'

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 2000,
      refetchOnWindowFocus: true,
      // Retrying a refused credential or a missing object only produces the
      // same answer three times more slowly.
      retry: (count, error) =>
        !(error instanceof ApiError && [401, 403, 404, 422].includes(error.status)) && count < 2,
    },
    mutations: { retry: false },
  },
})

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <TokenGate>
          <App />
        </TokenGate>
      </BrowserRouter>
    </QueryClientProvider>
  </StrictMode>,
)
