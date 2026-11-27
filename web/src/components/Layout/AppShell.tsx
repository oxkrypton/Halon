import React, { useState } from 'react';
import { NavLink, useNavigate } from 'react-router-dom';
import * as DropdownMenu from '@radix-ui/react-dropdown-menu';
import * as Dialog from '@radix-ui/react-dialog';
import {
  Activity,
  GitBranch,
  Route,
  FileText,
  Terminal,
  Sparkles,
  Shield,
  Users,
  Menu,
  Search,
  RefreshCw,
  LogOut,
  KeyRound,
  ChevronDown,
} from 'lucide-react';
import { useAuth } from '../../auth/AuthContext';
import { changeOwnPassword } from '../../api/auth';
import type { TimeWindow } from '../../api/observability';
import { StatusChip } from '../common/Primitives';

const WINDOWS: { value: TimeWindow; label: string }[] = [
  { value: '15m', label: '最近 15 分钟' },
  { value: '1h', label: '最近 1 小时' },
  { value: '6h', label: '最近 6 小时' },
  { value: '24h', label: '最近 24 小时' },
  { value: '7d', label: '最近 7 天' },
];

const REFRESH_OPTIONS: { value: string; label: string; ms: number | false }[] = [
  { value: 'off', label: '手动刷新', ms: false },
  { value: '15000', label: '15s', ms: 15_000 },
  { value: '30000', label: '30s', ms: 30_000 },
];

export function AppShell({ children }: { children: React.ReactNode }) {
  const { system, timeWindow, setTimeWindow, refreshIntervalMs, setRefreshIntervalMs, can, signOut } =
    useAuth();
  const [sidebarOpen, setSidebarOpen] = useState(true);
  const [searchText, setSearchText] = useState('');
  const [pwOpen, setPwOpen] = useState(false);
  const [curPw, setCurPw] = useState('');
  const [nextPw, setNextPw] = useState('');
  const [pwError, setPwError] = useState<string | null>(null);
  const [pwSaving, setPwSaving] = useState(false);
  const navigate = useNavigate();

  const handleGlobalSearch = (e: React.FormEvent) => {
    e.preventDefault();
    const q = searchText.trim();
    if (!q) return;
    if (/^[0-9a-fA-F]{32}$/.test(q) || /^[0-9a-fA-F]{16}$/.test(q)) {
      navigate(`/traces?trace_id=${encodeURIComponent(q)}`);
    } else if (/^\d{1,3}(\.\d{1,3}){3}$/.test(q) || q.includes(':')) {
      navigate(`/logs?client_ip=${encodeURIComponent(q)}`);
    } else if (q.startsWith('/')) {
      navigate(`/logs?path=${encodeURIComponent(q)}`);
    } else {
      navigate(`/logs?request_id=${encodeURIComponent(q)}`);
    }
  };

  const handleChangePassword = async (e: React.FormEvent) => {
    e.preventDefault();
    setPwError(null);
    setPwSaving(true);
    try {
      await changeOwnPassword(curPw, nextPw);
      setPwOpen(false);
      setCurPw('');
      setNextPw('');
      await signOut();
    } catch (err) {
      setPwError(err instanceof Error ? err.message : '修改密码失败');
    } finally {
      setPwSaving(false);
    }
  };

  const navItemClass = ({ isActive }: { isActive: boolean }) =>
    `flex items-center gap-3 rounded-r-full mr-3 px-4 py-2 text-[13px] font-medium transition-colors ${
      isActive
        ? 'bg-[#E8F0FE] text-[#1967D2]'
        : 'text-[#3C4043] hover:bg-[#F1F3F4]'
    }`;

  const canManageUsers = can('auth:user:read') && Boolean(system?.capabilities?.login_enabled);

  return (
    <div className="min-h-screen flex flex-col bg-[#F8F9FA]">
      {/* 56px Google Cloud style Top AppBar */}
      <header className="sticky top-0 z-30 h-14 bg-white border-b border-[#DADCE0] px-3 flex items-center justify-between gap-3">
        <div className="flex items-center gap-2 shrink-0">
          <button
            type="button"
            onClick={() => setSidebarOpen((v) => !v)}
            className="h-9 w-9 rounded-full flex items-center justify-center text-[#5F6368] hover:bg-[#F1F3F4] cursor-pointer"
            aria-label="切换导航栏"
          >
            <Menu className="h-5 w-5" />
          </button>
          <div className="flex items-center gap-2 pr-2">
            <div className="grid grid-cols-2 gap-0.5">
              <span className="h-2 w-2 rounded-full bg-[#4285F4]" />
              <span className="h-2 w-2 rounded-full bg-[#EA4335]" />
              <span className="h-2 w-2 rounded-full bg-[#FBBC05]" />
              <span className="h-2 w-2 rounded-full bg-[#34A853]" />
            </div>
            <span className="text-[17px] font-medium tracking-tight text-[#202124]">
              Halon <span className="font-normal text-[#5F6368]">Console</span>
            </span>
          </div>
        </div>

        {/* Google-style centered rounded search box */}
        <form onSubmit={handleGlobalSearch} className="flex-1 max-w-xl hidden md:block">
          <div className="relative">
            <Search className="absolute left-3.5 top-1/2 -translate-y-1/2 h-4 w-4 text-[#5F6368]" />
            <input
              type="text"
              value={searchText}
              onChange={(e) => setSearchText(e.target.value)}
              placeholder="搜索 Trace ID、Request ID、客户端 IP 或路径 (/api/...)"
              className="w-full rounded-lg bg-[#F1F3F4] border border-transparent pl-10 pr-4 py-1.5 text-[13px] text-[#202124] placeholder:text-[#5F6368] focus:bg-white focus:border-[#DADCE0] focus:shadow-sm focus:outline-none transition-all"
            />
          </div>
        </form>

        {/* Right controls */}
        <div className="flex items-center gap-2 shrink-0">
          <div className="hidden xl:flex items-center gap-1.5 mr-1">
            <StatusChip
              status={system?.health?.redis_up ? 'ok' : 'error'}
              label={system?.health?.redis_up ? 'Redis OK' : 'Redis 降级'}
            />
            <StatusChip
              status={system?.capabilities?.tempo_enabled ? 'info' : 'muted'}
              label={system?.capabilities?.tempo_enabled ? 'Tempo 已连接' : 'PG 追踪模式'}
            />
          </div>

          <select
            value={timeWindow}
            onChange={(e) => setTimeWindow(e.target.value as TimeWindow)}
            aria-label="时间窗口"
            className="rounded border border-[#DADCE0] bg-white px-2.5 py-1.5 text-[12.5px] text-[#202124] focus:border-[#1A73E8] focus:outline-none cursor-pointer"
          >
            {WINDOWS.map((w) => (
              <option key={w.value} value={w.value}>
                {w.label}
              </option>
            ))}
          </select>

          <div className="hidden sm:flex items-center rounded border border-[#DADCE0] bg-white px-2 py-1.5 text-[12px] text-[#5F6368] gap-1">
            <RefreshCw className={`h-3.5 w-3.5 ${refreshIntervalMs ? 'text-[#1A73E8]' : ''}`} />
            <select
              value={refreshIntervalMs === false ? 'off' : String(refreshIntervalMs)}
              onChange={(e) => {
                const found = REFRESH_OPTIONS.find((o) => o.value === e.target.value);
                setRefreshIntervalMs(found ? found.ms : false);
              }}
              aria-label="自动刷新"
              className="bg-transparent text-[#202124] focus:outline-none cursor-pointer"
            >
              {REFRESH_OPTIONS.map((o) => (
                <option key={o.value} value={o.value}>
                  {o.label}
                </option>
              ))}
            </select>
          </div>

          <DropdownMenu.Root>
            <DropdownMenu.Trigger asChild>
              <button
                type="button"
                className="flex items-center gap-2 rounded-full border border-[#DADCE0] bg-white pl-1.5 pr-2.5 py-1 hover:bg-[#F8F9FA] cursor-pointer"
              >
                <span className="h-6 w-6 rounded-full bg-[#1A73E8] text-white text-[12px] font-medium flex items-center justify-center uppercase">
                  {(system?.principal?.username || 'A').slice(0, 1)}
                </span>
                <span className="hidden sm:inline text-[12.5px] font-medium text-[#202124]">
                  {system?.principal?.username || 'admin'}
                </span>
                <span className="rounded bg-[#E8F0FE] px-1.5 py-0.5 text-[10.5px] font-mono text-[#1967D2]">
                  {system?.principal?.role || 'readonly'}
                </span>
                <ChevronDown className="h-3.5 w-3.5 text-[#5F6368]" />
              </button>
            </DropdownMenu.Trigger>
            <DropdownMenu.Portal>
              <DropdownMenu.Content
                align="end"
                sideOffset={6}
                className="z-50 w-52 rounded-lg border border-[#DADCE0] bg-white py-1.5 shadow-md"
              >
                <div className="px-3 py-2 border-b border-[#F1F3F4]">
                  <div className="text-[12.5px] font-medium text-[#202124]">
                    {system?.principal?.username}
                  </div>
                  <div className="text-[11px] text-[#5F6368] font-mono">
                    auth={system?.capabilities?.auth_mode} · role={system?.principal?.role}
                  </div>
                </div>
                {system?.principal?.user_id && (
                  <DropdownMenu.Item
                    onSelect={() => setPwOpen(true)}
                    className="flex items-center gap-2 px-3 py-2 text-[12.5px] text-[#202124] hover:bg-[#F1F3F4] cursor-pointer outline-none"
                  >
                    <KeyRound className="h-3.5 w-3.5 text-[#5F6368]" />
                    修改密码
                  </DropdownMenu.Item>
                )}
                <DropdownMenu.Item
                  onSelect={() => void signOut()}
                  className="flex items-center gap-2 px-3 py-2 text-[12.5px] text-[#D93025] hover:bg-[#FCE8E6]/50 cursor-pointer outline-none"
                >
                  <LogOut className="h-3.5 w-3.5" />
                  退出登录
                </DropdownMenu.Item>
              </DropdownMenu.Content>
            </DropdownMenu.Portal>
          </DropdownMenu.Root>
        </div>
      </header>

      {/* Body: Left Rail + Workspace */}
      <div className="flex flex-1 min-h-0">
        {sidebarOpen && (
          <aside className="w-[232px] shrink-0 bg-white border-r border-[#DADCE0] py-4 select-none">
            <div className="px-4 pb-1.5 text-[11px] font-medium uppercase tracking-wider text-[#5F6368]">
              观测与追踪
            </div>
            <nav className="space-y-0.5">
              <NavLink to="/" end className={navItemClass}>
                <Activity className="h-4 w-4" />
                概览看板
              </NavLink>
              <NavLink to="/traces" className={navItemClass}>
                <GitBranch className="h-4 w-4" />
                链路追踪 (Traces)
              </NavLink>
              <NavLink to="/routes" className={navItemClass}>
                <Route className="h-4 w-4" />
                路由与 P99 分析
              </NavLink>
              <NavLink to="/logs" className={navItemClass}>
                <FileText className="h-4 w-4" />
                请求与错误日志
              </NavLink>
              {can('k8s:pod_logs:read') && (
                <NavLink to="/pod-logs" className={navItemClass}>
                  <Terminal className="h-4 w-4" />
                  容器日志
                </NavLink>
              )}
              <NavLink to="/incidents" className={navItemClass}>
                <Sparkles className="h-4 w-4" />
                AI 故障报告
              </NavLink>
            </nav>

            <div className="px-4 pt-5 pb-1.5 text-[11px] font-medium uppercase tracking-wider text-[#5F6368]">
              网关管控
            </div>
            <nav className="space-y-0.5">
              <NavLink to="/gateway" className={navItemClass}>
                <Shield className="h-4 w-4" />
                熔断 · 黑名单 · 限流
              </NavLink>
              {canManageUsers && (
                <NavLink to="/users" className={navItemClass}>
                  <Users className="h-4 w-4" />
                  用户与权限
                </NavLink>
              )}
            </nav>
          </aside>
        )}

        <main className="flex-1 min-w-0 p-5 md:p-6 max-w-[1440px] w-full mx-auto">
          {children}
        </main>
      </div>

      {/* Change Own Password Dialog */}
      <Dialog.Root open={pwOpen} onOpenChange={setPwOpen}>
        <Dialog.Portal>
          <Dialog.Overlay className="fixed inset-0 bg-black/30 z-40" />
          <Dialog.Content className="fixed left-1/2 top-1/2 z-50 w-full max-w-md -translate-x-1/2 -translate-y-1/2 gc-card p-6 shadow-lg">
            <Dialog.Title className="text-[16px] font-medium text-[#202124]">修改当前账户密码</Dialog.Title>
            <Dialog.Description className="mt-1 text-[12.5px] text-[#5F6368]">
              修改成功后将吊销当前会话并要求重新登录。
            </Dialog.Description>
            {pwError && (
              <div className="mt-3 rounded bg-[#FCE8E6] px-3 py-2 text-[12px] text-[#C5221F]">{pwError}</div>
            )}
            <form onSubmit={handleChangePassword} className="mt-4 space-y-3">
              <div>
                <label className="block text-[12px] font-medium text-[#5F6368] mb-1">当前密码</label>
                <input
                  type="password"
                  required
                  value={curPw}
                  onChange={(e) => setCurPw(e.target.value)}
                  className="w-full rounded border border-[#DADCE0] px-3 py-2 text-[13px] focus:border-[#1A73E8] focus:outline-none"
                />
              </div>
              <div>
                <label className="block text-[12px] font-medium text-[#5F6368] mb-1">新密码</label>
                <input
                  type="password"
                  required
                  value={nextPw}
                  onChange={(e) => setNextPw(e.target.value)}
                  className="w-full rounded border border-[#DADCE0] px-3 py-2 text-[13px] focus:border-[#1A73E8] focus:outline-none"
                />
              </div>
              <div className="flex justify-end gap-2 pt-2">
                <button
                  type="button"
                  onClick={() => setPwOpen(false)}
                  className="rounded border border-[#DADCE0] px-4 py-1.5 text-[13px] text-[#3C4043] hover:bg-[#F1F3F4]"
                >
                  取消
                </button>
                <button
                  type="submit"
                  disabled={pwSaving}
                  className="rounded bg-[#1A73E8] px-4 py-1.5 text-[13px] font-medium text-white hover:bg-[#1765CC] disabled:opacity-50"
                >
                  {pwSaving ? '保存中...' : '确认修改'}
                </button>
              </div>
            </form>
          </Dialog.Content>
        </Dialog.Portal>
      </Dialog.Root>
    </div>
  );
}
