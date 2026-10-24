import { createRoot } from 'react-dom/client'
import App from './App'
import { ensureServiceWorker } from './lib/push'
import './index.css'

createRoot(document.getElementById('root')!).render(<App />)

// Register on boot so the PWA is installable before login. The worker does
// no runtime caching (see public/sw.js), so dev and HMR stay untouched.
void ensureServiceWorker()
