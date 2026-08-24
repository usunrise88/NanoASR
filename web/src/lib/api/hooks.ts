import { useCallback, useEffect, useRef, useState } from 'react'
import {
  useInfiniteQuery,
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
} from '@tanstack/react-query'

import {
  cancelJob,
  downloadPath,
  getJob,
  jobEventsPath,
  listCatalog,
  listJobs,
  listModels,
  loadModel,
  pinModel,
  reloadModel,
  submitJob,
  unloadModel,
} from './client'
import { stream } from './sse'
import { isTerminal, type DownloadProgress, type Job, type JobFilter, type ModelInfo } from './types'

/** Query keys, in one place so an invalidation cannot miss a reader. */
export const keys = {
  models: ['models'] as const,
  catalog: ['catalog'] as const,
  jobs: (filter: JobFilter) => ['jobs', filter] as const,
  job: (id: string) => ['job', id] as const,
}

export function useModels() {
  return useQuery({ queryKey: keys.models, queryFn: listModels })
}

export function useCatalog() {
  return useQuery({ queryKey: keys.catalog, queryFn: listCatalog })
}

/** One model by id, from the list — there is no per-model endpoint to call. */
export function useModel(id: string | undefined): ModelInfo | undefined {
  const { data } = useModels()
  return id ? data?.find((m) => m.id === id) : undefined
}

export function useJob(id: string, enabled = true) {
  return useQuery({ queryKey: keys.job(id), queryFn: () => getJob(id), enabled })
}

export function useJobHistory(filter: JobFilter) {
  return useInfiniteQuery({
    queryKey: keys.jobs(filter),
    queryFn: ({ pageParam }) =>
      listJobs(pageParam ? { ...filter, cursor: pageParam } : filter),
    initialPageParam: '',
    getNextPageParam: (page) => page.next_cursor ?? undefined,
  })
}

export function useSubmitJob(): UseMutationResult<
  Job,
  Error,
  { file: File; options: Parameters<typeof submitJob>[1] }
> {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ file, options }) => submitJob(file, options),
    onSuccess: (job) => {
      qc.setQueryData(keys.job(job.id), job)
      void qc.invalidateQueries({ queryKey: ['jobs'] })
    },
  })
}

export function useCancelJob(): UseMutationResult<Job, Error, string> {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: cancelJob,
    onSuccess: (job) => {
      qc.setQueryData(keys.job(job.id), job)
      void qc.invalidateQueries({ queryKey: ['jobs'] })
    },
  })
}

/** The model-state mutations, which differ only in which call they make. */
export function useModelAction<A extends unknown[]>(
  action: (...args: A) => Promise<ModelInfo>,
): UseMutationResult<ModelInfo, Error, A> {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (args: A) => action(...args),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: keys.models })
    },
  })
}

export const useLoadModel = () => useModelAction(loadModel)
export const useUnloadModel = () => useModelAction(unloadModel)
export const usePinModel = () => useModelAction(pinModel)
export const useReloadModel = () => useModelAction(reloadModel)

/**
 * Follows a job's transitions until it is terminal.
 *
 * The job arrives from the query cache first, so the screen has something to
 * draw before the stream connects, and every event overwrites the same cache
 * entry — meaning a page that reads the job through useJob updates too, without
 * knowing a stream exists.
 */
export function useJobStream(id: string | undefined, active: boolean): Job | undefined {
  const qc = useQueryClient()
  const [live, setLive] = useState<Job>()

  useEffect(() => {
    if (!id || !active) return
    const controller = new AbortController()

    void stream<Job>(jobEventsPath(id), {
      signal: controller.signal,
      onEvent: (_event, job) => {
        setLive(job)
        qc.setQueryData(keys.job(job.id), job)
      },
      onDone: () => {
        // The last event carried the terminal state, but the result itself is
        // only on the job endpoint; refetch once so the transcript is there.
        void qc.invalidateQueries({ queryKey: keys.job(id) })
        void qc.invalidateQueries({ queryKey: ['jobs'] })
      },
    })

    return () => controller.abort()
  }, [id, active, qc])

  return live
}

/** Whether a job still deserves a live connection. */
export function shouldWatch(job: Job | undefined): boolean {
  return job !== undefined && !isTerminal(job.status)
}

export interface DownloadState {
  progress: DownloadProgress | undefined
  error: string | undefined
  running: boolean
}

/**
 * Downloads a model, reporting progress.
 *
 * Not a mutation: the useful part is the stream of ticks, and react-query has
 * nowhere to put those. What it does share is the invalidation at the end, so
 * the model list reflects the new state.
 */
export function useModelDownload(): DownloadState & { start: (id: string) => void } {
  const qc = useQueryClient()
  const [state, setState] = useState<DownloadState>({
    progress: undefined,
    error: undefined,
    running: false,
  })
  const abort = useRef<AbortController>(undefined)

  useEffect(() => () => abort.current?.abort(), [])

  const start = useCallback(
    (id: string) => {
      abort.current?.abort()
      const controller = new AbortController()
      abort.current = controller
      setState({ progress: undefined, error: undefined, running: true })

      void stream<DownloadProgress>(downloadPath(id), {
        signal: controller.signal,
        onEvent: (_event, tick) => {
          setState({
            progress: tick,
            error: tick.error === '' ? undefined : tick.error,
            running: !tick.done,
          })
        },
        onError: (err) => {
          setState((s) => ({ ...s, error: String(err), running: false }))
        },
        onDone: () => {
          setState((s) => ({ ...s, running: false }))
          void qc.invalidateQueries({ queryKey: keys.models })
          void qc.invalidateQueries({ queryKey: keys.catalog })
        },
      })
    },
    [qc],
  )

  return { ...state, start }
}
