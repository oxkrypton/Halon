import React, { useEffect, useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import {
  Check,
  Copy,
  RefreshCw,
  Search,
  Server,
  Terminal,
  WrapText,
} from 'lucide-react';
import { useAuth } from '../auth/AuthContext';
import { ApiError } from '../api/client';
import { fetchPodLogs, fetchPods, type PodSummary } from '../api/pods';
import { Banner, EmptyState, StatusChip } from '../components/common/Primitives';
import { formatDateTime } from '../utils/format';

const TAIL_OPTIONS = [100, 200, 500, 1000] as const;
const SINCE_OPTIONS: { value: number; label: string }[] = [
  { value: 300, label: '最近 5 分钟' },
  { value: 900, label: '最近 15 分钟' },
  { value: 3600, label: '最近 1 小时' },
  { value: 21600, label: '最近 6 小时' },
  { value: 86400, label: '最近 24 小时' },
];

const HALON_LABEL_SELECTOR = 'app.kubernetes.io/part-of=halon';

type LogLevel = 'error' | 'warn' | 'info' | 'debug' | 'plain';

interface ParsedLogLine {
  raw: string;
  timestamp: string;
  level: LogLevel;
  body: string;
}

function podChipStatus(pod: PodSummary): 'ok' | 'warn' | 'error' | 'info' | 'muted' {
  if (pod.phase === 'Failed') return 'error';
  if (pod.ready && pod.phase === 'Running') return 'ok';
  if (pod.phase === 'Succeeded') return 'muted';
  if (pod.phase === 'Pending') return 'warn';
  return pod.restarts > 0 ? 'error' : 'warn';
}

function classifyLevel(rawLevel: unknown): LogLevel {
  if (typeof rawLevel !== 'string') return 'plain';
  switch (rawLevel.trim().toUpperCase()) {
    case 'ERROR':
    case 'ERR':
    case 'FATAL':
    case 'CRITICAL':
      return 'error';
    case 'WARN':
    case 'WARNING':
      return 'warn';
    case 'INFO':
      return 'info';
    case 'DEBUG':
    case 'TRACE':
      return 'debug';
    default:
      return 'plain';
  }
}

function parseLogLine(line: string): ParsedLogLine {
  let timestamp = '';
  let rest = line;
  const tsMatch = line.match(/^(\d{4}-\d{2}-\d{2}T\S+)\s+(.*)$/);
  if (tsMatch) {
    timestamp = tsMatch[1];
    rest = tsMatch[2];
  }

  let level: LogLevel = 'plain';
  const trimmed = rest.trim();
  if (trimmed.startsWith('{') && trimmed.endsWith('}')) {
    try {
      const parsed = JSON.parse(trimmed) as { level?: unknown };
      level = classifyLevel(parsed.level);
    } catch {
      level = 'plain';
    }
  } else if (/\b(ERROR|FATAL|PANIC)\b/i.test(rest)) {
    level = 'error';
  } else if (/\bWARN(ING)?\b/i.test(rest)) {
    level = 'warn';
  }

  return { raw: line, timestamp, level, body: rest };
}

function highlightText(text: string, query: string): React.ReactNode {
  if (!query) return text;
  const lowerText = text.toLowerCase();
  const lowerQuery = query.toLowerCase();
  const parts: React.ReactNode[] = [];
  let cursor = 0;
  let idx = lowerText.indexOf(lowerQuery, cursor);
  let key = 0;
  while (idx !== -1) {
    if (idx > cursor) {
      parts.push(text.slice(cursor, idx));
    }
    parts.push(
      <mark key={key++} className="rounded bg-[#FBBC04]/45 px-0.5 text-white">
        {text.slice(idx, idx + query.length)}
      </mark>,
    );
    cursor = idx + query.length;
    idx = lowerText.indexOf(lowerQuery, cursor);
  }
  if (cursor < text.length) {
    parts.push(text.slice(cursor));
  }
  return parts;
}

function levelLineClass(level: LogLevel): string {
  switch (level) {
    case 'error':
      return 'text-[#F28B82] bg-[#EA4335]/10';
    case 'warn':
      return 'text-[#FDD663] bg-[#FBBC04]/5';
    case 'info':
      return 'text-[#8AB4F8]';
    case 'debug':
      return 'text-[#9AA0A6]';
    default:
      return 'text-[#E8EAED]';
  }
}

export function PodLogsPage() {
  const { refreshIntervalMs } = useAuth();
  const [namespace, setNamespace] = useState('');
  const [customNsInput, setCustomNsInput] = useState('default');
  const [onlyHalon, setOnlyHalon] = useState(true);
  const [selectedPod, setSelectedPod] = useState('');
  const [selectedContainer, setSelectedContainer] = useState('');
  const [tailLines, setTailLines] = useState<number>(200);
  const [sinceSeconds, setSinceSeconds] = useState<number>(900);
  const [previous, setPrevious] = useState(false);
  const [keyword, setKeyword] = useState('');
  const [wrapLines, setWrapLines] = useState(true);
  const [copied, setCopied] = useState(false);

  const podsQ = useQuery({
    queryKey: ['k8s-pods', namespace, onlyHalon],
    queryFn: () =>
      fetchPods({
        namespace: namespace || undefined,
        labelSelector: onlyHalon ? HALON_LABEL_SELECTOR : undefined,
        limit: 100,
      }),
    refetchInterval: refreshIntervalMs,
    retry: false,
  });

  const allowedNamespaces = useMemo(() => podsQ.data?.namespaces || [], [podsQ.data?.namespaces]);
  const wildcardAllowed = allowedNamespaces.includes('*');
  const activeNamespace = podsQ.data?.namespace || namespace || 'default';
  const pods = useMemo(() => podsQ.data?.pods || [], [podsQ.data?.pods]);

  useEffect(() => {
    if (podsQ.data?.namespace && !namespace) {
      setNamespace(podsQ.data.namespace);
      setCustomNsInput(podsQ.data.namespace);
    }
  }, [podsQ.data?.namespace, namespace]);

  useEffect(() => {
    if (pods.length === 0) {
      setSelectedPod('');
      setSelectedContainer('');
      return;
    }
    const existing = pods.find((p) => p.name === selectedPod);
    if (!existing) {
      const first = pods[0];
      setSelectedPod(first.name);
      setSelectedContainer(first.containers[0]?.name || '');
      return;
    }
    if (
      existing.containers.length > 0 &&
      !existing.containers.some((c) => c.name === selectedContainer)
    ) {
      setSelectedContainer(existing.containers[0].name);
    }
  }, [pods, selectedPod, selectedContainer]);

  const activePod = useMemo(
    () => pods.find((p) => p.name === selectedPod) || null,
    [pods, selectedPod],
  );

  const logsQ = useQuery({
    queryKey: [
      'k8s-pod-logs',
      activeNamespace,
      selectedPod,
      selectedContainer,
      tailLines,
      sinceSeconds,
      previous,
    ],
    queryFn: () =>
      fetchPodLogs(selectedPod, {
        namespace: activeNamespace,
        container: selectedContainer || undefined,
        tailLines,
        sinceSeconds,
        previous,
      }),
    enabled: Boolean(selectedPod) && !podsQ.isError,
    refetchInterval: refreshIntervalMs,
    retry: false,
  });

  const rawLogs = logsQ.data?.logs ?? '';
  const allLines = useMemo(() => {
    if (!rawLogs) return [];
    const lines = rawLogs.split('\n');
    if (lines.length > 0 && lines[lines.length - 1] === '') {
      lines.pop();
    }
    return lines.map(parseLogLine);
  }, [rawLogs]);

  const trimmedKeyword = keyword.trim();
  const filteredLines = useMemo(() => {
    if (!trimmedKeyword) return allLines;
    const lower = trimmedKeyword.toLowerCase();
    return allLines.filter((item) => item.raw.toLowerCase().includes(lower));
  }, [allLines, trimmedKeyword]);

  const handleSelectPod = (pod: PodSummary) => {
    setSelectedPod(pod.name);
    setSelectedContainer(pod.containers[0]?.name || '');
  };

  const applyCustomNamespace = () => {
    const next = customNsInput.trim() || 'default';
    setCustomNsInput(next);
    setNamespace(next);
  };

  const handleCopy = async () => {
    const content = filteredLines.map((l) => l.raw).join('\n');
    if (!content) return;
    try {
      await navigator.clipboard.writeText(content);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1800);
    } catch {
      setCopied(false);
    }
  };

  const podsErr = podsQ.error instanceof ApiError ? podsQ.error : null;
  const logsErr = logsQ.error instanceof ApiError ? logsQ.error : null;

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="text-[20px] font-medium text-[#202124]">容器实时日志 (Pod Logs)</h1>
          <p className="text-[12.5px] text-[#5F6368]">
            通过 Agent 只读代理直连 Kubernetes API 查看容器 stdout 与上次崩溃实例日志
          </p>
        </div>
        <div className="flex items-center gap-2">
          <button
            type="button"
            onClick={() => {
              void podsQ.refetch();
              if (selectedPod) void logsQ.refetch();
            }}
            className="inline-flex items-center gap-1.5 rounded border border-[#DADCE0] bg-white px-3 py-1.5 text-[12.5px] font-medium text-[#3C4043] hover:bg-[#F8F9FA] cursor-pointer"
          >
            <RefreshCw
              className={`h-3.5 w-3.5 ${podsQ.isFetching || logsQ.isFetching ? 'animate-spin text-[#1A73E8]' : ''}`}
            />
            刷新
          </button>
        </div>
      </div>

      {podsQ.isError && (
        <Banner
          tone={podsErr?.status === 503 ? 'warn' : 'error'}
          title={
            podsErr?.status === 401
              ? 'Agent 鉴权失败'
              : podsErr?.status === 503
                ? 'Kubernetes 客户端不可用'
                : '获取 Pod 列表失败'
          }
          message={
            (podsQ.error as Error)?.message ||
            '请确认 Agent 已部署并具备目标命名空间只读权限。'
          }
        />
      )}

      <div className="grid grid-cols-1 lg:grid-cols-[330px_1fr] gap-4 items-start">
        {/* Left Panel: Namespace + Pod List */}
        <div className="gc-card overflow-hidden">
          <div className="p-3 border-b border-[#DADCE0] space-y-2.5 bg-[#F8F9FA]">
            <div className="flex items-center justify-between gap-2">
              <label className="text-[11.5px] font-medium text-[#5F6368] shrink-0">
                Namespace
              </label>
              {wildcardAllowed ? (
                <form
                  onSubmit={(e) => {
                    e.preventDefault();
                    applyCustomNamespace();
                  }}
                  className="flex items-center gap-1.5 flex-1 min-w-0"
                >
                  <input
                    type="text"
                    value={customNsInput}
                    onChange={(e) => setCustomNsInput(e.target.value)}
                    onBlur={applyCustomNamespace}
                    placeholder="default"
                    className="w-full min-w-0 rounded border border-[#DADCE0] bg-white px-2.5 py-1 text-[12px] font-mono text-[#202124] focus:border-[#1A73E8] focus:outline-none"
                  />
                  <button
                    type="submit"
                    className="rounded border border-[#DADCE0] bg-white px-2 py-1 text-[11.5px] text-[#3C4043] hover:bg-[#F1F3F4] cursor-pointer shrink-0"
                  >
                    切换
                  </button>
                </form>
              ) : (
                <select
                  value={activeNamespace}
                  onChange={(e) => setNamespace(e.target.value)}
                  className="flex-1 min-w-0 rounded border border-[#DADCE0] bg-white px-2.5 py-1 text-[12px] font-mono text-[#202124] focus:border-[#1A73E8] focus:outline-none cursor-pointer"
                >
                  {(allowedNamespaces.length > 0 ? allowedNamespaces : [activeNamespace]).map(
                    (ns) => (
                      <option key={ns} value={ns}>
                        {ns}
                      </option>
                    ),
                  )}
                </select>
              )}
            </div>

            <div className="flex items-center rounded-md border border-[#DADCE0] bg-white p-0.5 text-[11.5px]">
              <button
                type="button"
                onClick={() => setOnlyHalon(true)}
                className={`flex-1 rounded py-1 font-medium transition-colors cursor-pointer ${
                  onlyHalon
                    ? 'bg-[#E8F0FE] text-[#1967D2]'
                    : 'text-[#5F6368] hover:text-[#202124]'
                }`}
              >
                仅 Halon 组件
              </button>
              <button
                type="button"
                onClick={() => setOnlyHalon(false)}
                className={`flex-1 rounded py-1 font-medium transition-colors cursor-pointer ${
                  !onlyHalon
                    ? 'bg-[#E8F0FE] text-[#1967D2]'
                    : 'text-[#5F6368] hover:text-[#202124]'
                }`}
              >
                全部 Pod
              </button>
            </div>
          </div>

          <div className="divide-y divide-[#F1F3F4] max-h-[680px] overflow-y-auto">
            {podsQ.isLoading ? (
              <div className="py-10 text-center text-[12.5px] text-[#5F6368]">
                正在加载 Pod 列表...
              </div>
            ) : pods.length === 0 ? (
              <EmptyState
                title="暂无匹配 Pod"
                description={
                  onlyHalon
                    ? '当前命名空间没有带 app.kubernetes.io/part-of=halon 标签的 Pod，可尝试切换到「全部 Pod」。'
                    : '当前命名空间下暂无 Pod。'
                }
              />
            ) : (
              pods.map((pod) => {
                const isSelected = pod.name === selectedPod;
                const abnormalContainers = pod.containers.filter(
                  (c) => c.state !== 'running' && c.reason,
                );
                return (
                  <button
                    key={pod.name}
                    type="button"
                    onClick={() => handleSelectPod(pod)}
                    className={`w-full text-left px-3.5 py-3 transition-colors cursor-pointer ${
                      isSelected ? 'bg-[#E8F0FE]/75' : 'hover:bg-[#F8F9FA]'
                    }`}
                  >
                    <div className="flex items-center justify-between gap-2">
                      <span className="font-mono text-[12.5px] font-medium text-[#202124] truncate">
                        {pod.name}
                      </span>
                      <StatusChip
                        status={podChipStatus(pod)}
                        label={pod.ready ? 'Ready' : pod.phase || 'Unknown'}
                      />
                    </div>
                    <div className="mt-1.5 flex flex-wrap items-center justify-between gap-2 text-[11px] text-[#5F6368]">
                      <span className="inline-flex items-center gap-1 font-mono truncate">
                        <Server className="h-3 w-3 shrink-0" />
                        {pod.node || 'unscheduled'}
                      </span>
                      <span
                        className={`rounded px-1.5 py-0.5 font-mono tabular-nums ${
                          pod.restarts > 0
                            ? 'bg-[#FCE8E6] text-[#C5221F] font-medium'
                            : 'bg-[#F1F3F4] text-[#5F6368]'
                        }`}
                      >
                        重启 {pod.restarts}
                      </span>
                    </div>
                    {abnormalContainers.length > 0 && (
                      <div className="mt-1.5 flex flex-wrap gap-1">
                        {abnormalContainers.map((c) => (
                          <StatusChip
                            key={c.name}
                            status="warn"
                            label={`${c.name}: ${c.reason}`}
                          />
                        ))}
                      </div>
                    )}
                  </button>
                );
              })
            )}
          </div>
        </div>

        {/* Right Panel: Log Terminal */}
        <div className="gc-card overflow-hidden flex flex-col">
          {/* Toolbar */}
          <div className="p-3 border-b border-[#DADCE0] bg-[#F8F9FA] space-y-2.5">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <div className="flex items-center gap-2 min-w-0">
                <Terminal className="h-4 w-4 text-[#1A73E8] shrink-0" />
                <span className="font-mono text-[13px] font-medium text-[#202124] truncate">
                  {activePod ? `${activeNamespace}/${activePod.name}` : '未选择 Pod'}
                </span>
                {activePod?.created_at && (
                  <span className="hidden sm:inline text-[11px] text-[#80868B] font-mono">
                    创建于 {formatDateTime(activePod.created_at)}
                  </span>
                )}
              </div>

              <div className="flex flex-wrap items-center gap-2">
                <select
                  value={selectedContainer}
                  onChange={(e) => setSelectedContainer(e.target.value)}
                  disabled={!activePod || activePod.containers.length === 0}
                  aria-label="选择容器"
                  className="rounded border border-[#DADCE0] bg-white px-2.5 py-1 text-[12px] font-mono text-[#202124] disabled:opacity-50"
                >
                  {(activePod?.containers || []).map((c) => (
                    <option key={c.name} value={c.name}>
                      {c.name} ({c.state})
                    </option>
                  ))}
                </select>

                <select
                  value={tailLines}
                  onChange={(e) => setTailLines(Number(e.target.value))}
                  aria-label="尾部行数"
                  className="rounded border border-[#DADCE0] bg-white px-2.5 py-1 text-[12px] text-[#202124]"
                >
                  {TAIL_OPTIONS.map((n) => (
                    <option key={n} value={n}>
                      最近 {n} 行
                    </option>
                  ))}
                </select>

                <select
                  value={sinceSeconds}
                  onChange={(e) => setSinceSeconds(Number(e.target.value))}
                  aria-label="回溯时间"
                  className="rounded border border-[#DADCE0] bg-white px-2.5 py-1 text-[12px] text-[#202124]"
                >
                  {SINCE_OPTIONS.map((opt) => (
                    <option key={opt.value} value={opt.value}>
                      {opt.label}
                    </option>
                  ))}
                </select>
              </div>
            </div>

            <div className="flex flex-wrap items-center justify-between gap-2">
              <div className="relative flex-1 min-w-[200px]">
                <Search className="absolute left-2.5 top-1/2 -translate-y-1/2 h-3.5 w-3.5 text-[#5F6368]" />
                <input
                  type="text"
                  value={keyword}
                  onChange={(e) => setKeyword(e.target.value)}
                  placeholder="关键字过滤与高亮（如 trace_id、ERROR、timeout）"
                  className="w-full rounded border border-[#DADCE0] bg-white pl-8 pr-3 py-1 text-[12px] font-mono text-[#202124] placeholder:text-[#80868B] focus:border-[#1A73E8] focus:outline-none"
                />
              </div>

              <div className="flex items-center gap-2">
                <label className="inline-flex items-center gap-1.5 rounded border border-[#DADCE0] bg-white px-2.5 py-1 text-[12px] text-[#3C4043] cursor-pointer select-none">
                  <input
                    type="checkbox"
                    checked={previous}
                    onChange={(e) => setPrevious(e.target.checked)}
                    className="accent-[#1A73E8]"
                  />
                  上次崩溃实例 (previous)
                </label>

                <button
                  type="button"
                  onClick={() => setWrapLines((v) => !v)}
                  className={`inline-flex items-center gap-1 rounded border px-2.5 py-1 text-[12px] cursor-pointer ${
                    wrapLines
                      ? 'border-[#1A73E8] bg-[#E8F0FE] text-[#1967D2]'
                      : 'border-[#DADCE0] bg-white text-[#3C4043] hover:bg-[#F1F3F4]'
                  }`}
                >
                  <WrapText className="h-3.5 w-3.5" />
                  换行
                </button>

                <button
                  type="button"
                  onClick={() => void handleCopy()}
                  disabled={filteredLines.length === 0}
                  className="inline-flex items-center gap-1 rounded border border-[#DADCE0] bg-white px-2.5 py-1 text-[12px] text-[#3C4043] hover:bg-[#F1F3F4] disabled:opacity-50 cursor-pointer"
                >
                  {copied ? (
                    <>
                      <Check className="h-3.5 w-3.5 text-[#1E8E3E]" />
                      已复制
                    </>
                  ) : (
                    <>
                      <Copy className="h-3.5 w-3.5" />
                      复制
                    </>
                  )}
                </button>
              </div>
            </div>
          </div>

          {/* Log Body */}
          {logsQ.isError ? (
            <div className="p-4">
              <Banner
                tone={logsErr?.status === 400 || logsErr?.status === 404 ? 'warn' : 'error'}
                title={
                  logsErr?.status === 400 && previous
                    ? '未找到上次崩溃容器日志'
                    : '读取容器日志失败'
                }
                message={
                  logsErr?.status === 400 && previous
                    ? '该容器没有前序终止实例（previous terminated container），请取消勾选「上次崩溃实例」查看当前日志。'
                    : (logsQ.error as Error)?.message || '请稍后重试。'
                }
              />
            </div>
          ) : !selectedPod ? (
            <EmptyState
              title="请从左侧选择一个 Pod"
              description="选择 Pod 后将展示容器最近日志，支持关键字过滤与崩溃前日志查看。"
            />
          ) : logsQ.isLoading ? (
            <div className="py-16 text-center text-[12.5px] text-[#5F6368]">
              正在拉取容器日志...
            </div>
          ) : (
            <>
              <div className="flex items-center justify-between px-3.5 py-1.5 bg-[#28292C] text-[#9AA0A6] text-[11px] font-mono border-b border-[#3C4043]">
                <span>
                  container={logsQ.data?.container || selectedContainer || 'default'} ·{' '}
                  {previous ? 'previous=true' : 'current'} · 单次上限 1 MiB
                </span>
                <span>
                  {trimmedKeyword
                    ? `匹配 ${filteredLines.length} / ${allLines.length} 行`
                    : `共 ${allLines.length} 行`}
                </span>
              </div>

              {filteredLines.length === 0 ? (
                <div className="bg-[#202124] text-[#9AA0A6] py-16 text-center text-[12.5px] font-mono">
                  {trimmedKeyword
                    ? `没有匹配 "${trimmedKeyword}" 的日志行`
                    : '当前时间窗口内暂无日志输出'}
                </div>
              ) : (
                <div
                  className={`bg-[#202124] text-[#E8EAED] font-mono text-[12px] leading-[1.55] p-3 max-h-[620px] overflow-auto ${
                    wrapLines ? 'whitespace-pre-wrap break-all' : 'whitespace-pre'
                  }`}
                >
                  {filteredLines.map((line, index) => (
                    <div
                      key={index}
                      className={`px-1.5 py-0.5 rounded-sm ${levelLineClass(line.level)}`}
                    >
                      <span className="inline-block w-10 select-none text-right pr-3 text-[#5F6368] text-[11px]">
                        {index + 1}
                      </span>
                      {line.timestamp && (
                        <span className="text-[#9AA0A6] mr-2">
                          {highlightText(line.timestamp, trimmedKeyword)}
                        </span>
                      )}
                      <span>{highlightText(line.body, trimmedKeyword)}</span>
                    </div>
                  ))}
                </div>
              )}
            </>
          )}
        </div>
      </div>
    </div>
  );
}
