import React, { useEffect, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import * as Dialog from '@radix-ui/react-dialog';
import { Ban, CheckCircle2, ShieldAlert, Trash2 } from 'lucide-react';
import { useAuth } from '../auth/AuthContext';
import {
  blockIP,
  fetchBlacklist,
  fetchBreakers,
  fetchRateLimit,
  unblockIP,
  updateRateLimit,
} from '../api/gateway';
import { fetchTopIPs } from '../api/observability';
import { Banner, EmptyState, StatusChip } from '../components/common/Primitives';
import { formatCount, formatMS } from '../utils/format';

export function GatewayControlPage() {
  const { system, timeWindow, refreshIntervalMs, can } = useAuth();
  const queryClient = useQueryClient();

  const [newIP, setNewIP] = useState('');
  const [rpsInput, setRpsInput] = useState('');
  const [burstInput, setBurstInput] = useState('');
  const [confirmRateOpen, setConfirmRateOpen] = useState(false);
  const [feedback, setFeedback] = useState<{ tone: 'ok' | 'err'; msg: string } | null>(null);

  const canWriteBlacklist = can('gateway:blacklist:write');
  const canWriteRateLimit = can('gateway:ratelimit:write');

  const breakersQ = useQuery({
    queryKey: ['breakers'],
    queryFn: fetchBreakers,
    refetchInterval: refreshIntervalMs,
  });

  const blacklistQ = useQuery({
    queryKey: ['blacklist'],
    queryFn: fetchBlacklist,
    refetchInterval: refreshIntervalMs,
  });

  const rateLimitQ = useQuery({
    queryKey: ['rate-limit'],
    queryFn: fetchRateLimit,
    refetchInterval: refreshIntervalMs,
  });

  const topIPsQ = useQuery({
    queryKey: ['top-ips', timeWindow],
    queryFn: () => fetchTopIPs(timeWindow, '', 15),
    refetchInterval: refreshIntervalMs,
  });

  useEffect(() => {
    if (rateLimitQ.data) {
      setRpsInput(String(rateLimitQ.data.requests_per_second));
      setBurstInput(String(rateLimitQ.data.burst));
    }
  }, [rateLimitQ.data]);

  const blockMut = useMutation({
    mutationFn: (ip: string) => blockIP(ip),
    onSuccess: (res) => {
      setNewIP('');
      setFeedback({
        tone: 'ok',
        msg: `已封禁 ${res.ip}（Redis 事实源已写入${res.propagation ? `，传播窗口 ${res.propagation}` : ''}）。`,
      });
      void queryClient.invalidateQueries({ queryKey: ['blacklist'] });
    },
    onError: (err: Error) => setFeedback({ tone: 'err', msg: err.message }),
  });

  const unblockMut = useMutation({
    mutationFn: (ip: string) => unblockIP(ip),
    onSuccess: (_, ip) => {
      setFeedback({
        tone: 'ok',
        msg: `已解除封禁 ${ip}（全部副本将在 <=10s 内收敛生效）。`,
      });
      void queryClient.invalidateQueries({ queryKey: ['blacklist'] });
    },
    onError: (err: Error) => setFeedback({ tone: 'err', msg: err.message }),
  });

  const rateMut = useMutation({
    mutationFn: ({ rps, burst }: { rps: number; burst: number }) => updateRateLimit(rps, burst),
    onSuccess: (res) => {
      setConfirmRateOpen(false);
      setFeedback({
        tone: 'ok',
        msg: `全局限流已更新为 ${res.requests_per_second} req/s, burst=${res.burst}（10s 内同步至所有副本）。`,
      });
      void queryClient.invalidateQueries({ queryKey: ['rate-limit'] });
    },
    onError: (err: Error) => {
      setConfirmRateOpen(false);
      setFeedback({ tone: 'err', msg: err.message });
    },
  });

  const blockedSet = new Set(blacklistQ.data?.ips || []);
  const routesMap = new Map((system?.routes || []).map((r) => [r.prefix, r.upstream]));

  const handleAddIP = (e: React.FormEvent) => {
    e.preventDefault();
    if (!newIP.trim()) return;
    setFeedback(null);
    blockMut.mutate(newIP.trim());
  };

  return (
    <div className="space-y-5">
      <div>
        <h1 className="text-[20px] font-medium text-[#202124]">网关管控 (熔断 · 黑名单 · 限流)</h1>
        <p className="text-[12.5px] text-[#5F6368]">
          黑名单与限流由 Redis 集群共享（写后 ≤10s 全副本收敛）；熔断器反映当前响应副本的实时状态
        </p>
      </div>

      {!system?.health?.redis_up && (
        <Banner
          tone="error"
          title="Redis 不可达"
          message="共享状态 Redis 当前降级，黑名单与限流写操作将返回 503，读路径以本地最后缓存/fail-open 兜底。"
        />
      )}

      {feedback && (
        <div
          className={`flex items-center gap-2 rounded-lg border px-4 py-2.5 text-[12.5px] ${
            feedback.tone === 'ok'
              ? 'bg-[#E6F4EA] border-[#1E8E3E]/30 text-[#137333]'
              : 'bg-[#FCE8E6] border-[#D93025]/30 text-[#C5221F]'
          }`}
        >
          {feedback.tone === 'ok' ? (
            <CheckCircle2 className="h-4 w-4 shrink-0" />
          ) : (
            <ShieldAlert className="h-4 w-4 shrink-0" />
          )}
          <span>{feedback.msg}</span>
        </div>
      )}

      {/* Routes & Breakers */}
      <div className="gc-card overflow-hidden">
        <div className="px-4 py-3 border-b border-[#DADCE0] flex items-center justify-between">
          <div>
            <h2 className="text-[14px] font-medium text-[#202124]">路由表与熔断器状态</h2>
            <p className="text-[11.5px] text-[#5F6368]">per-replica 内存态熔断器（closed / half-open / open）</p>
          </div>
          <span className="text-[11.5px] font-mono text-[#5F6368]">
            {(breakersQ.data?.breakers || []).length} 条路由
          </span>
        </div>
        {(breakersQ.data?.breakers?.length || 0) === 0 ? (
          <EmptyState title="未配置代理路由 (ROUTES 为空)" />
        ) : (
          <table className="w-full text-left text-[12.5px]">
            <thead className="bg-[#F8F9FA] text-[11.5px] text-[#5F6368] border-b border-[#E8EAED]">
              <tr>
                <th className="px-4 py-2 font-medium">路由前缀</th>
                <th className="px-3 py-2 font-medium">上游地址</th>
                <th className="px-3 py-2 font-medium">熔断状态</th>
                <th className="px-4 py-2 font-medium text-right">剩余冷却时间</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-[#F1F3F4]">
              {(breakersQ.data?.breakers || []).map((b) => (
                <tr key={b.route} className="hover:bg-[#F8F9FA]">
                  <td className="px-4 py-2.5 font-mono text-[#202124] font-medium">{b.route}</td>
                  <td className="px-3 py-2.5 font-mono text-[#5F6368]">
                    {routesMap.get(b.route) || '—'}
                  </td>
                  <td className="px-3 py-2.5">
                    <StatusChip
                      status={
                        b.state === 'closed'
                          ? 'ok'
                          : b.state === 'open'
                            ? 'error'
                            : b.state === 'half-open'
                              ? 'warn'
                              : 'muted'
                      }
                      label={b.state}
                    />
                  </td>
                  <td className="px-4 py-2.5 text-right font-mono tabular-nums text-[#5F6368]">
                    {b.remaining_delay_seconds ? `${b.remaining_delay_seconds.toFixed(1)} s` : '—'}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {/* Blacklist + Rate Limit */}
      <div className="grid grid-cols-1 lg:grid-cols-2 gap-4 items-start">
        <div className="gc-card p-4 space-y-4">
          <div className="flex items-center justify-between">
            <div>
              <h2 className="text-[14px] font-medium text-[#202124]">IP 黑名单 (Redis 共享)</h2>
              <p className="text-[11.5px] text-[#5F6368]">需 operator 及以上权限写入</p>
            </div>
            <StatusChip status="info" label={`${blockedSet.size} 个 IP`} />
          </div>

          {canWriteBlacklist && (
            <form onSubmit={handleAddIP} className="flex gap-2">
              <input
                type="text"
                placeholder="输入 IPv4 / IPv6 地址，如 203.0.113.9"
                value={newIP}
                onChange={(e) => setNewIP(e.target.value)}
                className="flex-1 rounded border border-[#DADCE0] px-3 py-1.5 text-[12.5px] font-mono focus:border-[#1A73E8] focus:outline-none"
              />
              <button
                type="submit"
                disabled={blockMut.isPending}
                className="inline-flex items-center gap-1.5 rounded bg-[#D93025] px-3.5 py-1.5 text-[12.5px] font-medium text-white hover:bg-[#B31412] disabled:opacity-50 cursor-pointer"
              >
                <Ban className="h-3.5 w-3.5" />
                封禁 IP
              </button>
            </form>
          )}

          {blockedSet.size === 0 ? (
            <EmptyState title="当前黑名单为空" />
          ) : (
            <div className="divide-y divide-[#F1F3F4] rounded border border-[#E8EAED] max-h-72 overflow-y-auto">
              {(blacklistQ.data?.ips || []).map((ip) => (
                <div key={ip} className="flex items-center justify-between px-3 py-2 text-[12.5px]">
                  <span className="font-mono text-[#202124]">{ip}</span>
                  {canWriteBlacklist && (
                    <button
                      type="button"
                      onClick={() => unblockMut.mutate(ip)}
                      disabled={unblockMut.isPending}
                      className="inline-flex items-center gap-1 text-[12px] text-[#5F6368] hover:text-[#D93025] cursor-pointer"
                    >
                      <Trash2 className="h-3.5 w-3.5" />
                      解封
                    </button>
                  )}
                </div>
              ))}
            </div>
          )}
        </div>

        <div className="gc-card p-4 space-y-4">
          <div>
            <h2 className="text-[14px] font-medium text-[#202124]">全局单 IP 令牌桶限流 (Redis Lua)</h2>
            <p className="text-[11.5px] text-[#5F6368]">
              多副本合计限流量严格等于配置值 · 仅 full 权限可修改
            </p>
          </div>

          <div className="grid grid-cols-2 gap-3">
            <div>
              <label className="block text-[11.5px] font-medium text-[#5F6368] mb-1">
                速率 (Requests / sec)
              </label>
              <input
                type="number"
                step="0.5"
                min="0.1"
                disabled={!canWriteRateLimit}
                value={rpsInput}
                onChange={(e) => setRpsInput(e.target.value)}
                className="w-full rounded border border-[#DADCE0] px-3 py-1.5 text-[13px] font-mono disabled:bg-[#F8F9FA]"
              />
            </div>
            <div>
              <label className="block text-[11.5px] font-medium text-[#5F6368] mb-1">突发容量 (Burst)</label>
              <input
                type="number"
                min="1"
                disabled={!canWriteRateLimit}
                value={burstInput}
                onChange={(e) => setBurstInput(e.target.value)}
                className="w-full rounded border border-[#DADCE0] px-3 py-1.5 text-[13px] font-mono disabled:bg-[#F8F9FA]"
              />
            </div>
          </div>

          {canWriteRateLimit && (
            <div className="flex justify-end">
              <button
                type="button"
                onClick={() => setConfirmRateOpen(true)}
                className="rounded bg-[#1A73E8] px-4 py-1.5 text-[12.5px] font-medium text-white hover:bg-[#1765CC] cursor-pointer"
              >
                应用限流参数
              </button>
            </div>
          )}
        </div>
      </div>

      {/* Top Client IPs */}
      <div className="gc-card overflow-hidden">
        <div className="px-4 py-3 border-b border-[#DADCE0]">
          <h2 className="text-[14px] font-medium text-[#202124]">活跃客户端 IP 排行 (当前时间窗口)</h2>
        </div>
        {(topIPsQ.data?.clients?.length || 0) === 0 ? (
          <EmptyState title="暂无客户端流量记录" />
        ) : (
          <table className="w-full text-left text-[12.5px]">
            <thead className="bg-[#F8F9FA] text-[11.5px] text-[#5F6368] border-b border-[#E8EAED]">
              <tr>
                <th className="px-4 py-2 font-medium">客户端 IP</th>
                <th className="px-3 py-2 font-medium text-right">请求数</th>
                <th className="px-3 py-2 font-medium text-right">4xx/5xx 错误</th>
                <th className="px-3 py-2 font-medium text-right">5xx</th>
                <th className="px-3 py-2 font-medium text-right">Avg / P95</th>
                <th className="px-4 py-2 font-medium text-right">操作</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-[#F1F3F4]">
              {(topIPsQ.data?.clients || []).map((c) => {
                const blocked = blockedSet.has(c.client_ip);
                return (
                  <tr key={c.client_ip} className="hover:bg-[#F8F9FA]">
                    <td className="px-4 py-2 font-mono text-[#202124]">
                      {c.client_ip}{' '}
                      {blocked && <StatusChip status="error" label="已封禁" />}
                    </td>
                    <td className="px-3 py-2 text-right font-mono tabular-nums">{formatCount(c.requests)}</td>
                    <td className="px-3 py-2 text-right font-mono tabular-nums">{c.error_count}</td>
                    <td className="px-3 py-2 text-right font-mono tabular-nums text-[#D93025]">{c.status_5xx}</td>
                    <td className="px-3 py-2 text-right font-mono tabular-nums text-[#5F6368]">
                      {formatMS(c.avg_ms)} / {formatMS(c.p95_ms)}
                    </td>
                    <td className="px-4 py-2 text-right">
                      {canWriteBlacklist && (
                        <button
                          type="button"
                          onClick={() => (blocked ? unblockMut.mutate(c.client_ip) : blockMut.mutate(c.client_ip))}
                          className={`text-[12px] font-medium cursor-pointer hover:underline ${
                            blocked ? 'text-[#1A73E8]' : 'text-[#D93025]'
                          }`}
                        >
                          {blocked ? '解封' : '加入黑名单'}
                        </button>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </div>

      {/* Rate limit confirm dialog */}
      <Dialog.Root open={confirmRateOpen} onOpenChange={setConfirmRateOpen}>
        <Dialog.Portal>
          <Dialog.Overlay className="fixed inset-0 bg-black/30 z-40" />
          <Dialog.Content className="fixed left-1/2 top-1/2 z-50 w-full max-w-md -translate-x-1/2 -translate-y-1/2 gc-card p-6 shadow-lg">
            <Dialog.Title className="text-[16px] font-medium text-[#202124]">确认更新全局限流参数</Dialog.Title>
            <Dialog.Description className="mt-2 text-[13px] text-[#5F6368]">
              将全集群限流调整为 <strong className="font-mono text-[#202124]">{rpsInput} req/s</strong>，突发{' '}
              <strong className="font-mono text-[#202124]">{burstInput}</strong>。过低的限流阈值可能导致正常业务被大面积限流。
            </Dialog.Description>
            <div className="mt-5 flex justify-end gap-2">
              <button
                type="button"
                onClick={() => setConfirmRateOpen(false)}
                className="rounded border border-[#DADCE0] px-4 py-1.5 text-[13px] text-[#3C4043] hover:bg-[#F1F3F4]"
              >
                取消
              </button>
              <button
                type="button"
                disabled={rateMut.isPending}
                onClick={() =>
                  rateMut.mutate({ rps: Number(rpsInput), burst: Number(burstInput) })
                }
                className="rounded bg-[#1A73E8] px-4 py-1.5 text-[13px] font-medium text-white hover:bg-[#1765CC]"
              >
                {rateMut.isPending ? '应用中...' : '确认修改'}
              </button>
            </div>
          </Dialog.Content>
        </Dialog.Portal>
      </Dialog.Root>
    </div>
  );
}
