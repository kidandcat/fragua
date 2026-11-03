import re,sys,math,json
src=open(sys.argv[1]).read()
tok=re.findall(r'\(|\)|"(?:[^"\\]|\\.)*"|[^\s()]+',src)
def parse(i):
    out=[];i+=1
    while tok[i]!=')':
        if tok[i]=='(':
            n,i=parse(i);out.append(n)
        else:
            t=tok[i];out.append(t[1:-1] if t.startswith('"') else t);i+=1
    return out,i+1
root,_=parse(0)
def kids(n,name): return [c for c in n if isinstance(c,list) and c and c[0]==name]
def kid(n,name):
    k=kids(n,name);return k[0] if k else None
fps=kids(root,'footprint')
libs={};parts=[];nets={}
for fp in fps:
    name=fp[1].split(':')[1]
    props={p[1]:p[2] for p in kids(fp,'property')}
    ref=props.get('Reference');val=props.get('Value','')
    at=kid(fp,'at');rot=float(at[3]) if len(at)>3 else 0
    layer=kid(fp,'layer')[1]
    attr=kid(fp,'attr')
    pads=[]
    for p in kids(fp,'pad'):
        num=p[1];typ=p[2]
        if num=='' : continue
        pa=kid(p,'at');x,y=float(pa[1]),float(pa[2]);pr=(float(pa[3]) if len(pa)>3 else 0)-rot
        sz=kid(p,'size');w,h=float(sz[1]),float(sz[2])
        if round(pr/90)%2: w,h=h,w
        # pad at is in board-rotated frame in kicad files? no: local coords relative, rotation absolute
        d=kid(p,'drill');dr=None
        if d:
            nums=[float(v) for v in d[1:] if not isinstance(v,list) and re.match(r'^[\d.]+$',v)]
            dr=min(nums) if nums else None
        n=kid(p,'net')
        pads.append((num,x,-y,w,h,dr,n[2] if n and len(n)>2 else (n[1] if n else None),typ))
    th=[q for q in pads if q[7]=='thru_hole']
    def dup(q): return q[7]!='thru_hole' and any(abs(q[1]-t[1])<0.05 and abs(q[2]-t[2])<0.05 for t in th)
    pads=[q for q in pads if not dup(q)]
    if not pads: continue
    key='k_'+re.sub(r'[^A-Za-z0-9]','_',name).lower()
    if key not in libs:
        seen=set();pl=[]
        for (num,x,y,w,h,dr,_,typ) in pads:
            k=num
            while k in seen:k+='_'
            seen.add(k);pl.append((k,x,y,w,h,dr))
        libs[key]=pl
    parts.append((ref,key,val,rot,layer,pads))
    seen=set()
    for (num,*r) in pads:
        net=r[5]
        if net and not net.startswith('unconnected'):
            nets.setdefault(net,set()).add(f'{ref}.{num}')
L=[]
for k,pl in libs.items():
    L.append(f'lib {k}')
    for (n,x,y,w,h,dr) in pl:
        L.append(f'  pad {n} {x:.3f} {y:.3f} {w:.3f} {h:.3f}'+(f' drill={dr}' if dr else ''))
for ref,key,val,rot,layer,pads in parts:
    L.append(f'sym {ref} ic key={key}')
    s=set()
    for p in pads:
        if p[0] in s: continue
        s.add(p[0]);L.append(f'  pin {p[0]} L P{p[0]} role=passive')
for ref,key,val,rot,layer,pads in parts:
    v=re.sub(r'\s','',val) or '-'
    L.append(f'palette {ref} {key} value={v}')
for n,pins in sorted(nets.items()):
    if len(pins)<2: continue
    nn=re.sub(r'[^A-Za-z0-9_+.-]','_',n.lstrip('/')) or 'N'
    cls=' class=ground' if nn.upper()=='GND' else ''
    L.append(f'net {nn} '+' '.join(sorted(pins))+cls)
print('\n'.join(L))
sys.stderr.write(f'parts={len(parts)} libs={len(libs)} nets={sum(1 for p in nets.values() if len(p)>1)}\n')
