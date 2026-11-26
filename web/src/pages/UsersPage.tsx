import React, { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import * as Dialog from '@radix-ui/react-dialog';
import { Copy, KeyRound, Plus, UserCheck, UserX } from 'lucide-react';
import { useAuth } from '../auth/AuthContext';
import type { Role } from '../api/auth';
import { createUser, fetchUsers, resetUserPassword, updateUser, type UserView } from '../api/gateway';
import { Banner, EmptyState, StatusChip } from '../components/common/Primitives';
import { formatDateTime } from '../utils/format';

export function UsersPage() {
  const { system, can } = useAuth();
  const queryClient = useQueryClient();

  const [createOpen, setCreateOpen] = useState(false);
  const [username, setUsername] = useState('');
  const [displayName, setDisplayName] = useState('');
  const [role, setRole] = useState<Role>('readonly');
  const [password, setPassword] = useState('');
  const [generatedSecret, setGeneratedSecret] = useState<{ username: string; password: string } | null>(null);
  const [errorMsg, setErrorMsg] = useState<string | null>(null);

  const canManage = can('auth:user:manage');

  const usersQ = useQuery({
    queryKey: ['users'],
    queryFn: () => fetchUsers(0, 100),
    enabled: can('auth:user:read'),
  });

  const createMut = useMutation({
    mutationFn: createUser,
    onSuccess: (res) => {
      setCreateOpen(false);
      setUsername('');
      setDisplayName('');
      setPassword('');
      setErrorMsg(null);
      if (res.generated_password) {
        setGeneratedSecret({ username: res.user.username, password: res.generated_password });
      }
      void queryClient.invalidateQueries({ queryKey: ['users'] });
    },
    onError: (err: Error) => setErrorMsg(err.message),
  });

  const updateMut = useMutation({
    mutationFn: ({ id, patch }: { id: string; patch: { role?: Role; disabled?: boolean } }) =>
      updateUser(id, patch),
    onSuccess: () => {
      setErrorMsg(null);
      void queryClient.invalidateQueries({ queryKey: ['users'] });
    },
    onError: (err: Error) => setErrorMsg(err.message),
  });

  const resetPwMut = useMutation({
    mutationFn: (u: UserView) => resetUserPassword(u.id).then((res) => ({ user: u, res })),
    onSuccess: ({ user, res }) => {
      setErrorMsg(null);
      setGeneratedSecret({ username: user.username, password: res.generated_password });
      void queryClient.invalidateQueries({ queryKey: ['users'] });
    },
    onError: (err: Error) => setErrorMsg(err.message),
  });

  if (!can('auth:user:read')) {
    return <Banner tone="warn" title="权限不足" message="仅 full 角色管理员可查看与管理用户。" />;
  }

  const handleCreate = (e: React.FormEvent) => {
    e.preventDefault();
    setErrorMsg(null);
    createMut.mutate({
      username: username.trim(),
      role,
      display_name: displayName.trim() || undefined,
      password: password || undefined,
    });
  };

  const users = usersQ.data?.users || [];

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="text-[20px] font-medium text-[#202124]">用户与权限管理</h1>
          <p className="text-[12.5px] text-[#5F6368]">
            管理本地账号角色（readonly / operator / full）、启禁用状态与临时密码签发
          </p>
        </div>
        {canManage && (
          <button
            type="button"
            onClick={() => {
              setErrorMsg(null);
              setCreateOpen(true);
            }}
            className="inline-flex items-center gap-1.5 rounded bg-[#1A73E8] px-3.5 py-2 text-[12.5px] font-medium text-white hover:bg-[#1765CC] cursor-pointer"
          >
            <Plus className="h-4 w-4" />
            新建用户
          </button>
        )}
      </div>

      {errorMsg && <Banner tone="error" title="操作失败" message={errorMsg} />}

      <div className="gc-card overflow-x-auto">
        {users.length === 0 ? (
          <EmptyState title="暂无本地用户" />
        ) : (
          <table className="w-full text-left text-[12.5px]">
            <thead className="bg-[#F8F9FA] text-[11.5px] text-[#5F6368] border-b border-[#DADCE0]">
              <tr>
                <th className="px-4 py-2.5 font-medium">用户名</th>
                <th className="px-3 py-2.5 font-medium">显示名</th>
                <th className="px-3 py-2.5 font-medium">角色</th>
                <th className="px-3 py-2.5 font-medium">状态</th>
                <th className="px-3 py-2.5 font-medium">创建时间</th>
                <th className="px-4 py-2.5 font-medium text-right">操作</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-[#F1F3F4]">
              {users.map((u) => {
                const isSelf = u.id === system?.principal?.user_id;
                return (
                  <tr key={u.id} className="hover:bg-[#F8F9FA]">
                    <td className="px-4 py-2.5 font-mono font-medium text-[#202124]">
                      {u.username}{' '}
                      {isSelf && <StatusChip status="info" label="当前登录" />}
                    </td>
                    <td className="px-3 py-2.5 text-[#5F6368]">{u.display_name || '—'}</td>
                    <td className="px-3 py-2.5">
                      <select
                        disabled={!canManage || isSelf || updateMut.isPending}
                        value={u.role}
                        onChange={(e) =>
                          updateMut.mutate({ id: u.id, patch: { role: e.target.value as Role } })
                        }
                        className="rounded border border-[#DADCE0] bg-white px-2 py-1 text-[12px] font-mono disabled:bg-[#F8F9FA]"
                      >
                        <option value="readonly">readonly</option>
                        <option value="operator">operator</option>
                        <option value="full">full</option>
                      </select>
                    </td>
                    <td className="px-3 py-2.5 space-x-1.5">
                      <StatusChip status={u.disabled ? 'error' : 'ok'} label={u.disabled ? '已禁用' : '启用'} />
                      {u.must_change_password && <StatusChip status="warn" label="待改密" />}
                    </td>
                    <td className="px-3 py-2.5 text-[11.5px] text-[#5F6368] tabular-nums">
                      {formatDateTime(u.created_at)}
                    </td>
                    <td className="px-4 py-2.5 text-right space-x-3 whitespace-nowrap">
                      {canManage && (
                        <>
                          <button
                            type="button"
                            onClick={() => resetPwMut.mutate(u)}
                            className="inline-flex items-center gap-1 text-[12px] text-[#1A73E8] hover:underline cursor-pointer"
                          >
                            <KeyRound className="h-3.5 w-3.5" />
                            重置密码
                          </button>
                          {!isSelf && (
                            <button
                              type="button"
                              onClick={() =>
                                updateMut.mutate({ id: u.id, patch: { disabled: !u.disabled } })
                              }
                              className={`inline-flex items-center gap-1 text-[12px] hover:underline cursor-pointer ${
                                u.disabled ? 'text-[#1E8E3E]' : 'text-[#D93025]'
                              }`}
                            >
                              {u.disabled ? (
                                <>
                                  <UserCheck className="h-3.5 w-3.5" /> 启用
                                </>
                              ) : (
                                <>
                                  <UserX className="h-3.5 w-3.5" /> 禁用
                                </>
                              )}
                            </button>
                          )}
                        </>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </div>

      {/* Create User Modal */}
      <Dialog.Root open={createOpen} onOpenChange={setCreateOpen}>
        <Dialog.Portal>
          <Dialog.Overlay className="fixed inset-0 bg-black/30 z-40" />
          <Dialog.Content className="fixed left-1/2 top-1/2 z-50 w-full max-w-md -translate-x-1/2 -translate-y-1/2 gc-card p-6 shadow-lg">
            <Dialog.Title className="text-[16px] font-medium text-[#202124]">新建本地用户</Dialog.Title>
            <Dialog.Description className="mt-1 text-[12.5px] text-[#5F6368]">
              留空密码时将自动生成高强度临时密码，并强制用户首次登录后修改。
            </Dialog.Description>
            <form onSubmit={handleCreate} className="mt-4 space-y-3">
              <div>
                <label className="block text-[12px] font-medium text-[#5F6368] mb-1">用户名</label>
                <input
                  type="text"
                  required
                  placeholder="alice"
                  value={username}
                  onChange={(e) => setUsername(e.target.value)}
                  className="w-full rounded border border-[#DADCE0] px-3 py-1.5 text-[13px] font-mono"
                />
              </div>
              <div>
                <label className="block text-[12px] font-medium text-[#5F6368] mb-1">显示名（可选）</label>
                <input
                  type="text"
                  placeholder="Alice Chen"
                  value={displayName}
                  onChange={(e) => setDisplayName(e.target.value)}
                  className="w-full rounded border border-[#DADCE0] px-3 py-1.5 text-[13px]"
                />
              </div>
              <div>
                <label className="block text-[12px] font-medium text-[#5F6368] mb-1">角色</label>
                <select
                  value={role}
                  onChange={(e) => setRole(e.target.value as Role)}
                  className="w-full rounded border border-[#DADCE0] bg-white px-3 py-1.5 text-[13px] font-mono"
                >
                  <option value="readonly">readonly (只读观测与报告)</option>
                  <option value="operator">operator (含 IP 黑名单写入)</option>
                  <option value="full">full (含限流与用户管理)</option>
                </select>
              </div>
              <div>
                <label className="block text-[12px] font-medium text-[#5F6368] mb-1">初始密码（留空自动生成）</label>
                <input
                  type="password"
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                  className="w-full rounded border border-[#DADCE0] px-3 py-1.5 text-[13px]"
                />
              </div>
              <div className="flex justify-end gap-2 pt-2">
                <button
                  type="button"
                  onClick={() => setCreateOpen(false)}
                  className="rounded border border-[#DADCE0] px-4 py-1.5 text-[13px]"
                >
                  取消
                </button>
                <button
                  type="submit"
                  disabled={createMut.isPending}
                  className="rounded bg-[#1A73E8] px-4 py-1.5 text-[13px] font-medium text-white hover:bg-[#1765CC]"
                >
                  {createMut.isPending ? '创建中...' : '创建'}
                </button>
              </div>
            </form>
          </Dialog.Content>
        </Dialog.Portal>
      </Dialog.Root>

      {/* One-time Generated Password Modal */}
      <Dialog.Root open={Boolean(generatedSecret)} onOpenChange={() => setGeneratedSecret(null)}>
        <Dialog.Portal>
          <Dialog.Overlay className="fixed inset-0 bg-black/30 z-40" />
          <Dialog.Content className="fixed left-1/2 top-1/2 z-50 w-full max-w-md -translate-x-1/2 -translate-y-1/2 gc-card p-6 shadow-lg">
            <Dialog.Title className="text-[16px] font-medium text-[#202124]">一次性临时密码</Dialog.Title>
            <Dialog.Description className="mt-1 text-[12.5px] text-[#5F6368]">
              请立即复制并交付给用户 <strong className="font-mono">{generatedSecret?.username}</strong>，关闭弹窗后无法再次查看。
            </Dialog.Description>
            <div className="mt-4 flex items-center justify-between gap-2 rounded bg-[#F1F3F4] border border-[#DADCE0] px-3 py-2.5 font-mono text-[13px]">
              <span className="break-all select-all">{generatedSecret?.password}</span>
              <button
                type="button"
                onClick={() => {
                  if (generatedSecret) {
                    void navigator.clipboard.writeText(generatedSecret.password);
                  }
                }}
                className="shrink-0 rounded p-1.5 hover:bg-white text-[#1A73E8] cursor-pointer"
                title="复制密码"
              >
                <Copy className="h-4 w-4" />
              </button>
            </div>
            <div className="mt-5 flex justify-end">
              <button
                type="button"
                onClick={() => setGeneratedSecret(null)}
                className="rounded bg-[#1A73E8] px-4 py-1.5 text-[13px] font-medium text-white"
              >
                我已保存
              </button>
            </div>
          </Dialog.Content>
        </Dialog.Portal>
      </Dialog.Root>
    </div>
  );
}
