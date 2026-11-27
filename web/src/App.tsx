import { BrowserRouter, Navigate, Route, Routes } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { AuthProvider, useAuth } from './auth/AuthContext';
import { LoginPage } from './auth/LoginPage';
import { AppShell } from './components/Layout/AppShell';
import { OverviewPage } from './pages/OverviewPage';
import { TracesPage } from './pages/TracesPage';
import { RoutesPage } from './pages/RoutesPage';
import { LogsPage } from './pages/LogsPage';
import { PodLogsPage } from './pages/PodLogsPage';
import { IncidentsPage } from './pages/IncidentsPage';
import { GatewayControlPage } from './pages/GatewayControlPage';
import { UsersPage } from './pages/UsersPage';

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 10_000,
      retry: 1,
      refetchOnWindowFocus: false,
    },
  },
});

function ConsoleRoutes() {
  const { status } = useAuth();

  if (status === 'loading') {
    return (
      <div className="min-h-screen bg-[#F8F9FA] flex items-center justify-center text-[13px] text-[#5F6368]">
        正在加载 Halon Console...
      </div>
    );
  }

  if (status !== 'authenticated') {
    return <LoginPage />;
  }

  return (
    <AppShell>
      <Routes>
        <Route path="/" element={<OverviewPage />} />
        <Route path="/traces" element={<TracesPage />} />
        <Route path="/routes" element={<RoutesPage />} />
        <Route path="/logs" element={<LogsPage />} />
        <Route path="/pod-logs" element={<PodLogsPage />} />
        <Route path="/incidents" element={<IncidentsPage />} />
        <Route path="/gateway" element={<GatewayControlPage />} />
        <Route path="/users" element={<UsersPage />} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
    </AppShell>
  );
}

export function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <AuthProvider>
          <ConsoleRoutes />
        </AuthProvider>
      </BrowserRouter>
    </QueryClientProvider>
  );
}

export default App;
