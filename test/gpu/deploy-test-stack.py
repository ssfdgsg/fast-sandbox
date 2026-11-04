#!/usr/bin/env python3
"""Deploy the test stack into a dedicated cluster after offline image imports."""
import argparse,json,subprocess
from pathlib import Path
p=argparse.ArgumentParser()
p.add_argument('--config',type=Path,required=True)
p.add_argument('--socket',required=True)
p.add_argument('--containerd-root',required=True)
p.add_argument('--kubelet-root',default='/var/lib/kubelet')
p.add_argument('--nvidia-runtime-class',default='nvidia')
p.add_argument('--output',type=Path,required=True)
a=p.parse_args();a.output.mkdir(parents=True,exist_ok=True)
def run(args,obj=None):
 r=subprocess.run(['kubectl','--request-timeout=30s',*args],input=json.dumps(obj) if obj is not None else None,text=True,capture_output=True,timeout=40)
 with (a.output/'deployment.log').open('a') as f:f.write(json.dumps({'args':args,'rc':r.returncode,'stdout':r.stdout,'stderr':r.stderr})+'\n')
 if r.returncode:raise RuntimeError(r.stderr)
 return r.stdout
# Refuse duplicate NVIDIA allocators in any namespace before mutating the cluster.
existing=json.loads(run(['get','daemonsets','--all-namespaces','-o','json']))
for item in existing['items']:
 for c in item['spec']['template']['spec'].get('containers',[]):
  image=c.get('image','').lower()
  allocator=('k8s-device-plugin' in image or 'nvidia-device-plugin' in image)
  own=(item['metadata']['name']=='fsb-gpu-device-plugin' and item['metadata']['namespace']=='kube-system')
  if allocator and not own:raise RuntimeError('Existing NVIDIA plugin found in '+item['metadata']['namespace']+'; inspect/use its exclusive UUID configuration instead of deploying a second allocator')
# These manifests use public development route keys: dedicated test cluster only.
run(['apply','-k',str(a.config/'dev')])
value={'version':'v1alpha2','environments':{'default':{'containerd':{'socket':a.socket,'namespace':'k8s.io','defaultSnapshotter':'overlayfs','root':a.containerd_root},'kubelet':{'root':a.kubelet_root},'runtimes':{'container':{'fastletOverhead':{'cpu':'100m','memory':'128Mi'}}}}}}
run(['-n','fast-sandbox-system','patch','configmap','fast-sandbox-runtime-environments','--type=merge','-p',json.dumps({'data':{'runtime-environments.yaml':json.dumps(value)}})])
# Janitor's socket/FIFO hostPath is explicit; the immutable Fastlet plan comes from the ConfigMap above.
patch={'spec':{'template':{'spec':{'volumes':[{'name':'sock','hostPath':{'path':a.socket,'type':'Socket'}},{'name':'fifo','hostPath':{'path':str(Path(a.socket).parent/'fifo'),'type':'DirectoryOrCreate'}}]}}}}
run(['-n','fast-sandbox-system','patch','daemonset','fast-sandbox-janitor','--type=strategic','-p',json.dumps(patch)])
run(['-n','fast-sandbox-system','patch','daemonset','fast-sandbox-janitor','--type=strategic','-p',json.dumps({'spec':{'template':{'spec':{'containers':[{'name':'janitor','args':['--scan-interval=10s','--orphan-timeout=10s']}]}}}})])
janitor=json.loads(run(['-n','fast-sandbox-system','get','daemonset','fast-sandbox-janitor','-o','json']))
mounts=janitor['spec']['template']['spec']['containers'][0]['volumeMounts']
for mount in mounts:
 if mount['name']=='sock':mount['mountPath']=a.socket
 if mount['name']=='fifo':mount['mountPath']=str(Path(a.socket).parent/'fifo')
run(['-n','fast-sandbox-system','patch','daemonset','fast-sandbox-janitor','--type=json','-p',json.dumps([{'op':'replace','path':'/spec/template/spec/containers/0/volumeMounts','value':mounts}])])
run(['-n','fast-sandbox-system','rollout','restart','daemonset/fast-sandbox-janitor'])
run(['get','runtimeclass',a.nvidia_runtime_class])
plugin={'apiVersion':'apps/v1','kind':'DaemonSet','metadata':{'name':'fsb-gpu-device-plugin','namespace':'kube-system'},'spec':{'selector':{'matchLabels':{'app':'fsb-gpu-device-plugin'}},'template':{'metadata':{'labels':{'app':'fsb-gpu-device-plugin'}},'spec':{'runtimeClassName':a.nvidia_runtime_class,'tolerations':[{'operator':'Exists'}],'containers':[{'name':'plugin','image':'nvcr.io/nvidia/k8s-device-plugin:v0.17.1','imagePullPolicy':'Never','env':[{'name':'NVIDIA_VISIBLE_DEVICES','value':'all'},{'name':'NVIDIA_DRIVER_CAPABILITIES','value':'compute,utility'}],'args':['--mig-strategy=none','--device-id-strategy=uuid','--device-list-strategy=cdi-cri','--fail-on-init-error=true'],'securityContext':{'allowPrivilegeEscalation':False,'capabilities':{'drop':['ALL']}},'volumeMounts':[{'name':'device-plugin','mountPath':'/var/lib/kubelet/device-plugins'}]}],'volumes':[{'name':'device-plugin','hostPath':{'path':str(Path(a.kubelet_root)/'device-plugins'),'type':'Directory'}}]}}}}
# Native CDI generation discovers driver files through a read-only host root and
# publishes its separate k8s.device-plugin.nvidia.com specifications on the node.
plugin['spec']['template']['spec']['containers'][0]['volumeMounts'].extend([
 {'name':'driver-root','mountPath':'/driver-root','readOnly':True},
 {'name':'cdi-specs','mountPath':'/var/run/cdi'}])
plugin['spec']['template']['spec']['volumes'].extend([
 {'name':'driver-root','hostPath':{'path':'/','type':'Directory'}},
 {'name':'cdi-specs','hostPath':{'path':'/var/run/cdi','type':'DirectoryOrCreate'}}])
run(['apply','-f','-'],plugin)
print('Stack submitted. Wait for deployments, Janitor, device plugin and two allocatable GPUs; then run cluster-smoke.py.')
