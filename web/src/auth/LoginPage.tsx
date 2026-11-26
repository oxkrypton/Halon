import React, { useState } from 'react';
import * as Tabs from '@radix-ui/react-tabs';
import { KeyRound, Lock, ShieldCheck, AlertCircle, CheckCircle2 } from 'lucide-react';
import { ApiError } from '../api/client';
import { useAuth } from './AuthContext';

function GoogleFourDotMark() {
  return (
    <div className="inline-flex items-center gap-1.5">
      <span className="h-2.5 w-2.5 rounded-full bg-[#4285F4]" />
      <span className="h-2.5 w-2.5 rounded-full bg-[#EA4335]" />
      <span className="h-2.5 w-2.5 rounded-full bg-[#FBBC05]" />
      <span className="h-2.5 w-2.5 rounded-full bg-[#34A853]" />
    </div>
  );
}

export function LoginPage() {
  const { status, system, signInWithPassword, signInWithToken, completeForcedPasswordChange, signOut } =
    useAuth();
  const [tab, setTab] = useState<'account' | 'token'>('account');
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [bearerToken, setBearerToken] = useState('');
  const [currentPw, setCurrentPw] = useState('');
  const [newPw, setNewPw] = useState('');
  const [confirmPw, setConfirmPw] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  const handlePasswordLogin = async (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);
    setNotice(null);
    setSubmitting(true);
    try {
      await signInWithPassword(username.trim(), password);
    } catch (err) {
      if (err instanceof ApiError && err.code === 'LOGIN_DISABLED') {
        setTab('token');
        setNotice('当前网关未开启本地账号登录 (AUTH_LOGIN_ENABLED=false)，请使用 Bearer Token 登录。');
      } else {
        setError(err instanceof Error ? err.message : '登录失败');
      }
    } finally {
      setSubmitting(false);
    }
  };

  const handleTokenLogin = async (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);
    setNotice(null);
    if (!bearerToken.trim()) {
      setError('请输入有效的 ADMIN_TOKEN 或 Ed25519 JWT');
      return;
    }
    setSubmitting(true);
    try {
      await signInWithToken(bearerToken);
    } catch (err) {
      setError(err instanceof Error ? err.message : '令牌验证失败');
    } finally {
      setSubmitting(false);
    }
  };

  const handleForcedPasswordChange = async (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);
    if (newPw !== confirmPw) {
      setError('两次输入的新密码不一致');
      return;
    }
    setSubmitting(true);
    try {
      await completeForcedPasswordChange(currentPw, newPw);
      setNotice('密码已更新，请使用新密码重新登录。');
      setPassword('');
      setCurrentPw('');
      setNewPw('');
      setConfirmPw('');
    } catch (err) {
      setError(err instanceof Error ? err.message : '修改密码失败');
    } finally {
      setSubmitting(false);
    }
  };

  if (status === 'password_change_required') {
    return (
      <div className="min-h-screen bg-[#F8F9FA] flex items-center justify-center p-4">
        <div className="gc-card w-full max-w-[420px] p-8">
          <div className="flex flex-col items-center text-center mb-6">
            <GoogleFourDotMark />
            <h1 className="mt-4 text-[22px] font-medium text-[#202124]">更新您的初始密码</h1>
            <p className="mt-1.5 text-[13px] text-[#5F6368]">
              账号 <span className="font-mono font-medium text-[#202124]">{system?.principal?.username}</span>{' '}
              需要修改密码后才能继续访问控制台。
            </p>
          </div>
          {error && (
            <div className="mb-4 flex items-start gap-2 rounded-md bg-[#FCE8E6] px-3 py-2.5 text-[12.5px] text-[#C5221F]">
              <AlertCircle className="h-4 w-4 mt-0.5 shrink-0" />
              <span>{error}</span>
            </div>
          )}
          <form onSubmit={handleForcedPasswordChange} className="space-y-4">
            <div>
              <label className="block text-[12px] font-medium text-[#5F6368] mb-1">当前密码</label>
              <input
                type="password"
                required
                value={currentPw}
                onChange={(e) => setCurrentPw(e.target.value)}
                className="w-full rounded border border-[#DADCE0] px-3 py-2 text-[13px] focus:border-[#1A73E8] focus:outline-none"
              />
            </div>
            <div>
              <label className="block text-[12px] font-medium text-[#5F6368] mb-1">新密码</label>
              <input
                type="password"
                required
                value={newPw}
                onChange={(e) => setNewPw(e.target.value)}
                className="w-full rounded border border-[#DADCE0] px-3 py-2 text-[13px] focus:border-[#1A73E8] focus:outline-none"
              />
            </div>
            <div>
              <label className="block text-[12px] font-medium text-[#5F6368] mb-1">确认新密码</label>
              <input
                type="password"
                required
                value={confirmPw}
                onChange={(e) => setConfirmPw(e.target.value)}
                className="w-full rounded border border-[#DADCE0] px-3 py-2 text-[13px] focus:border-[#1A73E8] focus:outline-none"
              />
            </div>
            <div className="flex items-center justify-between pt-2">
              <button
                type="button"
                onClick={() => void signOut()}
                className="text-[13px] font-medium text-[#1A73E8] hover:underline"
              >
                返回登录
              </button>
              <button
                type="submit"
                disabled={submitting}
                className="rounded bg-[#1A73E8] px-5 py-2 text-[13px] font-medium text-white hover:bg-[#1765CC] disabled:opacity-50"
              >
                {submitting ? '提交中...' : '确认修改'}
              </button>
            </div>
          </form>
        </div>
      </div>
    );
  }

  return (
    <div className="min-h-screen bg-[#F8F9FA] flex items-center justify-center p-4">
      <div className="gc-card w-full max-w-[430px] p-8">
        <div className="flex flex-col items-center text-center mb-6">
          <GoogleFourDotMark />
          <h1 className="mt-4 text-[22px] font-medium text-[#202124]">Halon Cloud Console</h1>
          <p className="mt-1 text-[13px] text-[#5F6368]">网关观测、分布式链路追踪与运维控制台</p>
        </div>

        {notice && (
          <div className="mb-4 flex items-start gap-2 rounded-md bg-[#E8F0FE] px-3 py-2.5 text-[12.5px] text-[#1967D2]">
            <CheckCircle2 className="h-4 w-4 mt-0.5 shrink-0" />
            <span>{notice}</span>
          </div>
        )}
        {error && (
          <div className="mb-4 flex items-start gap-2 rounded-md bg-[#FCE8E6] px-3 py-2.5 text-[12.5px] text-[#C5221F]">
            <AlertCircle className="h-4 w-4 mt-0.5 shrink-0" />
            <span>{error}</span>
          </div>
        )}

        <Tabs.Root
          value={tab}
          onValueChange={(v) => {
            setTab(v as 'account' | 'token');
            setError(null);
          }}
        >
          <Tabs.List className="grid grid-cols-2 border-b border-[#DADCE0] mb-5">
            <Tabs.Trigger
              value="account"
              className="flex items-center justify-center gap-1.5 py-2.5 text-[13px] font-medium text-[#5F6368] border-b-2 border-transparent data-[state=active]:border-[#1A73E8] data-[state=active]:text-[#1A73E8] cursor-pointer"
            >
              <Lock className="h-3.5 w-3.5" />
              账号密码登录
            </Tabs.Trigger>
            <Tabs.Trigger
              value="token"
              className="flex items-center justify-center gap-1.5 py-2.5 text-[13px] font-medium text-[#5F6368] border-b-2 border-transparent data-[state=active]:border-[#1A73E8] data-[state=active]:text-[#1A73E8] cursor-pointer"
            >
              <KeyRound className="h-3.5 w-3.5" />
              Bearer Token
            </Tabs.Trigger>
          </Tabs.List>

          <Tabs.Content value="account">
            <form onSubmit={handlePasswordLogin} className="space-y-4">
              <div>
                <label className="block text-[12px] font-medium text-[#5F6368] mb-1">用户名</label>
                <input
                  type="text"
                  autoComplete="username"
                  required
                  placeholder="admin"
                  value={username}
                  onChange={(e) => setUsername(e.target.value)}
                  className="w-full rounded border border-[#DADCE0] px-3 py-2 text-[13px] focus:border-[#1A73E8] focus:outline-none"
                />
              </div>
              <div>
                <label className="block text-[12px] font-medium text-[#5F6368] mb-1">密码</label>
                <input
                  type="password"
                  autoComplete="current-password"
                  required
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                  className="w-full rounded border border-[#DADCE0] px-3 py-2 text-[13px] focus:border-[#1A73E8] focus:outline-none"
                />
              </div>
              <button
                type="submit"
                disabled={submitting}
                className="w-full rounded bg-[#1A73E8] py-2.5 text-[13px] font-medium text-white hover:bg-[#1765CC] disabled:opacity-50 cursor-pointer"
              >
                {submitting ? '正在验证...' : '登录'}
              </button>
              <p className="text-[11.5px] text-[#80868B] text-center">
                需网关开启 <code className="font-mono">AUTH_LOGIN_ENABLED=true</code>
              </p>
            </form>
          </Tabs.Content>

          <Tabs.Content value="token">
            <form onSubmit={handleTokenLogin} className="space-y-4">
              <div>
                <label className="block text-[12px] font-medium text-[#5F6368] mb-1">
                  ADMIN_TOKEN 或 Ed25519 JWT
                </label>
                <textarea
                  rows={4}
                  required
                  placeholder="粘贴静态 ADMIN_TOKEN（legacy/hybrid）或 halonctl 签发的 JWT"
                  value={bearerToken}
                  onChange={(e) => setBearerToken(e.target.value)}
                  className="w-full rounded border border-[#DADCE0] px-3 py-2 font-mono text-[12px] focus:border-[#1A73E8] focus:outline-none"
                />
              </div>
              <button
                type="submit"
                disabled={submitting}
                className="w-full flex items-center justify-center gap-1.5 rounded bg-[#1A73E8] py-2.5 text-[13px] font-medium text-white hover:bg-[#1765CC] disabled:opacity-50 cursor-pointer"
              >
                <ShieldCheck className="h-4 w-4" />
                {submitting ? '正在校验...' : '使用令牌进入'}
              </button>
            </form>
          </Tabs.Content>
        </Tabs.Root>
      </div>
    </div>
  );
}
