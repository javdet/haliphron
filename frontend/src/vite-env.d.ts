/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** The release this bundle was built for; the image build sets it from VERSION. */
  readonly VITE_HALIPHRON_VERSION?: string
}
