export async function handler(event, context) {
  return {
    ok: true,
    event,
    region: context.region,
    hostId: context.hostId,
    attemptId: context.attemptId,
  };
}
