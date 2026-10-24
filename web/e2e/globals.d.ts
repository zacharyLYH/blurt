declare interface ExtendableEventInit extends EventInit {
  bubbles?: boolean
  cancelable?: boolean
  composed?: boolean
}

declare interface ExtendableEvent extends Event {
  waitUntil(f: Promise<unknown>): void
}

declare type PushMessageDataInit = BufferSource | string

declare interface PushMessageData {
  arrayBuffer(): Promise<ArrayBuffer>
  json(): unknown
  text(): Promise<string>
}

declare interface PushEventInit extends ExtendableEventInit {
  data?: PushMessageDataInit
}

declare interface PushEvent extends ExtendableEvent {
  readonly data: PushMessageData | null
}

declare const PushEvent: {
  prototype: PushEvent
  new (type: string, eventInitDict?: PushEventInit): PushEvent
}
