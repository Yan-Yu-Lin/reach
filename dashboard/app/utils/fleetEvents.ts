export type FleetEventPlan = {
  fullRefresh: boolean
  machineId: string | null
  notifyPending: boolean
}

export function planFleetEvent(event: string, rawData: unknown): FleetEventPlan {
  const data = rawData && typeof rawData === 'object'
    ? rawData as Record<string, unknown>
    : {}
  const machineId = typeof data.machine_id === 'string' && data.machine_id
    ? data.machine_id
    : null

  if (event === 'hello' || event.startsWith('request.') || event === 'ssh_config.changed') {
    return {
      fullRefresh: true,
      machineId: null,
      notifyPending: event === 'request.created' && data.status === 'pending',
    }
  }

  if (event.startsWith('machine.') || event.startsWith('agent.')) {
    return {
      fullRefresh: machineId === null,
      machineId,
      notifyPending: false,
    }
  }

  return { fullRefresh: false, machineId: null, notifyPending: false }
}
